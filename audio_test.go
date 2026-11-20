package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestSanitizeFileName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"1. Origins", "1. Origins"},
		{"Interlude: The Prehistory of Computing", "Interlude- The Prehistory of Computing"},
		{"a/b\\c:d*e?f\"g<h>i|j", "a-b-c-d-e-f-g-h-i-j"},
		{"  weird \n spaces\t", "weird spaces"},
		{".hidden", "hidden"},
		{"...", "chapter"},
		{"", "chapter"},
		{"\x01\x02", "chapter"},
		{strings.Repeat("x", 200), strings.Repeat("x", maxTitleLen)},
		{"emoji ✓ ok", "emoji ✓ ok"},
	}
	for _, c := range cases {
		if got := sanitizeFileName(c.in); got != c.want {
			t.Errorf("sanitizeFileName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// A separator must never survive: it would move the file out of the book folder.
	for _, in := range []string{"../../etc/passwd", "..\\..\\windows", "/"} {
		if got := sanitizeFileName(in); strings.ContainsAny(got, `/\`) {
			t.Errorf("sanitizeFileName(%q) = %q keeps a separator", in, got)
		}
	}
}

type chapterProbe struct {
	Title string
	Start float64
}

func probeChapters(t *testing.T, file string) []chapterProbe {
	t.Helper()
	cmd := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_chapters", "-print_format", "json", file)
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("ffprobe chapters: %v", err)
	}
	var out struct {
		Chapters []struct {
			StartTime string `json:"start_time"`
			Tags      struct {
				Title string `json:"title"`
			} `json:"tags"`
		} `json:"chapters"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("ffprobe output %s: %v", raw, err)
	}
	var chapters []chapterProbe
	for _, c := range out.Chapters {
		start, err := strconv.ParseFloat(c.StartTime, 64)
		if err != nil {
			t.Fatalf("chapter start %q: %v", c.StartTime, err)
		}
		chapters = append(chapters, chapterProbe{Title: c.Tags.Title, Start: start})
	}
	return chapters
}

func probeDurationOf(t *testing.T, file string) float64 {
	t.Helper()
	cmd := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration",
		"-of", "default=nw=1:nk=1", file)
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("ffprobe duration: %v", err)
	}
	var d float64
	if _, err := fmt.Sscanf(strings.TrimSpace(string(raw)), "%f", &d); err != nil {
		t.Fatalf("ffprobe duration %s: %v", raw, err)
	}
	return d
}

func requireFFMpeg(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
}

// endToEndFixture sets up an input folder with the fixture book and an engine
// server that answers every segment with a one second sine wav. Silence
// between segments is off so the assembled durations are exact.
func endToEndFixture(t *testing.T) (cfg config, src, out, fixture string, eng *ttsEngine, fake *fakeEngine) {
	t.Helper()
	requireFFMpeg(t)
	dir := t.TempDir()
	src = filepath.Join(dir, "in")
	out = filepath.Join(dir, "out")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	fixture = writeFixtureEpub(t, filepath.Join(src, "Fixture Book.epub"), true)
	fake, srv := newFakeEngine(t)
	cfg = defaultConfig()
	cfg.FolderIn = src
	cfg.FolderOut = out
	cfg.TTSSpeaker = "test_voice"
	cfg.TTSURL = srv.URL
	cfg.TTSMaxChars = 4000 // one segment per paragraph: predictable durations
	cfg.TTSSilenceMS = 0
	cfg.Bitrate = "64k"
	eng = newTTSEngine(cfg)
	t.Cleanup(eng.stop)
	return
}

func TestConvertBookEndToEnd(t *testing.T) {
	cfg, src, out, fixture, eng, fake := endToEndFixture(t)

	b := &Book{EpubPath: fixture, BookName: "Fixture Book.epub", SourceFolder: src}
	if err := convertBook(b, cfg, eng); err != nil {
		t.Fatalf("convertBook: %v", err)
	}
	if b.Chapters != 2 {
		t.Errorf("book.Chapters = %d, want 2", b.Chapters)
	}

	bookDir := filepath.Join(out, "Fixture Book")
	var names []string
	entries, err := os.ReadDir(bookDir)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{"Fixture Book.epub", "Fixture Book.m4b"}
	if len(names) != len(want) {
		t.Fatalf("output = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("output %d = %q, want %q", i, names[i], want[i])
		}
	}

	// The engine must have seen one request per segment, all for the
	// configured narrator, and never the file path of the source epub.
	reqs := fake.requestLog()
	if len(reqs) < 2 {
		t.Fatalf("engine saw %d requests, want at least one per chapter", len(reqs))
	}
	n1, n2 := 0, 0
	for _, req := range reqs {
		if req.Speaker != "test_voice" {
			t.Errorf("request speaker = %q", req.Speaker)
		}
		if strings.Contains(req.Text, fixture) || strings.HasSuffix(req.Text, ".epub") {
			t.Errorf("engine was sent a file path: %q", req.Text)
		}
		if strings.Contains(req.Text, "First the world") || strings.Contains(req.Text, "Chapter One") {
			n1++
		}
		if strings.Contains(req.Text, "Learning is a prediction") || strings.Contains(req.Text, "Chapter Two") {
			n2++
		}
	}
	if n1 == 0 || n2 == 0 {
		t.Fatalf("the chapters did not reach the engine as expected: %d/%d segments", n1, n2)
	}

	// The m4b has to carry the chapter names, at the right offsets. Every
	// segment is one second long, so chapter two starts after n1 seconds.
	chs := probeChapters(t, filepath.Join(bookDir, "Fixture Book.m4b"))
	if len(chs) != 2 {
		t.Fatalf("m4b has %d chapters: %+v", len(chs), chs)
	}
	if chs[0].Title != "Chapter One" || chs[1].Title != "Chapter Two" {
		t.Errorf("m4b chapter titles = %q, %q", chs[0].Title, chs[1].Title)
	}
	if start, want := chs[1].Start, float64(n1); start < want-0.2 || start > want+0.2 {
		t.Errorf("second chapter starts at %.3fs, want ~%.1fs", start, want)
	}
	total := probeDurationOf(t, filepath.Join(bookDir, "Fixture Book.m4b"))
	if want := float64(n1 + n2); total < want-0.3 || total > want+0.3 {
		t.Errorf("m4b duration %.3fs, want ~%.1fs", total, want)
	}

	// The source epub is copied as the finished marker.
	if fi, err := os.Stat(filepath.Join(bookDir, "Fixture Book.epub")); err != nil || fi.Size() == 0 {
		t.Errorf("finished epub missing: %v", err)
	}
	// Nothing left behind in the work directory.
	if leftovers, _ := filepath.Glob(filepath.Join(os.TempDir(), "audiobookconverter", "*", "Fixture Book", "*.wav")); len(leftovers) > 0 {
		t.Errorf("wav files left behind: %v", leftovers)
	}
}

// The copy of the epub in the output folder is the marker of a finished book:
// nothing may be written there before the audio arrived.
func TestIncompleteBookIsNotMarkedConverted(t *testing.T) {
	tests := []struct {
		name    string
		spoiler func(bookDir string)
		flake   bool // the engine gives up after the first segment
	}{
		{
			name:  "the engine dies after the first segment",
			flake: true,
		},
		{
			name: "the audio cannot be written to the output folder",
			spoiler: func(bookDir string) {
				// The copy of the audiobook cannot succeed (a directory of that
				// name is in the way) while the epub marker could be written.
				if err := os.MkdirAll(filepath.Join(bookDir, "Fixture Book.m4b"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, src, out, fixture, eng, fake := endToEndFixture(t)
			if tc.flake {
				fake.stopAfter = 1
			}
			bookDir := filepath.Join(out, "Fixture Book")
			if tc.spoiler != nil {
				tc.spoiler(bookDir)
			}
			b := &Book{EpubPath: fixture, BookName: "Fixture Book.epub", SourceFolder: src}
			if err := convertBook(b, cfg, eng); err == nil {
				t.Fatal("convertBook reported success")
			}
			if _, err := os.Stat(filepath.Join(bookDir, "Fixture Book.epub")); err == nil {
				t.Error("the converted marker was written although the book is incomplete")
			}
		})
	}
}

// A retried book must not keep the audio of an earlier attempt - including the
// per-chapter mp3 files an older converter wrote - but it keeps the epub.
func TestMoveFilesReplacesStaleAudio(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	work := func(name string) {
		if err := os.WriteFile(filepath.Join(src, name), []byte("new"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"Book.m4b", "chapters.wav", "chapters.txt", "chapters.epub", "cover"} {
		work(n)
	}
	stale := filepath.Join(dst, "1 - Chapter One.mp3")
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The cover of an earlier attempt is not audio and is excluded anyway.
	if err := os.WriteFile(filepath.Join(dst, "cover"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := moveFiles(src, dst, map[string]bool{
		"chapters.epub": true, "cover": true, "chapters.txt": true,
	}, []string{".wav", ".txt"}); err != nil {
		t.Fatalf("moveFiles: %v", err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("the mp3 of an earlier attempt survived")
	}
	if _, err := os.Stat(filepath.Join(dst, "Book.m4b")); err != nil {
		t.Errorf("the new audiobook was not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "cover")); err != nil {
		t.Errorf("an excluded file was touched: %v", err)
	}
	for _, e := range []string{"chapters.wav", "chapters.txt", "chapters.epub"} {
		if _, err := os.Stat(filepath.Join(dst, e)); err == nil {
			t.Errorf("%s was copied to the output folder", e)
		}
	}
}
