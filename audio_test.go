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

func TestConversionStatusScan(t *testing.T) {
	st := &conversionStatus{}
	if !st.scan("Estimated time remaining (assuming 90 chars/sec): 00d 04h 12m 30s") {
		t.Error("eta line was not recognised")
	}
	if percent, eta := st.snapshot(); eta != "00d 04h 12m 30s" || percent != 0 {
		t.Errorf("snapshot = %d%%, %q", percent, eta)
	}
	if !st.scan("Progress: 42%") {
		t.Error("progress line was not recognised")
	}
	if percent, eta := st.snapshot(); percent != 42 || eta != "00d 04h 12m 30s" {
		t.Errorf("snapshot = %d%%, %q", percent, eta)
	}
	// Lines we do not understand must not disturb the state.
	if st.scan("Downloading kokoro model") {
		t.Error("unknown line reported as progress")
	}
	if percent, eta := st.snapshot(); percent != 42 || eta != "00d 04h 12m 30s" {
		t.Errorf("unknown line changed state: %d%%, %q", percent, eta)
	}
	// The eta format of the pypi release (which is what pip installs).
	other := &conversionStatus{}
	other.scan("Estimated time remaining: 1h 2m 3s")
	if _, eta := other.snapshot(); eta != "1h 2m 3s" {
		t.Errorf("eta = %q", eta)
	}
}

// fakeAudiblez writes a stub that produces one wav per spine document, named
// exactly like audiblez names its output, using the argument list it was called
// with. The wavs are sine tones of increasing length, so tests can check that
// chapters end up in the right order with the right titles.
func fakeAudiblez(t *testing.T, argsFile string) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	dir := t.TempDir()
	script := `#!/usr/bin/env python3
import os, re, subprocess, sys, zipfile
args = sys.argv[1:]
with open(os.environ["FAKE_ARGS_FILE"], "w") as fh:
    fh.write("\n".join(args))
epub = [a for a in args if a.endswith(".epub")][-1]
voice = args[args.index("-v") + 1]
base = os.path.splitext(os.path.basename(epub))[0]
with zipfile.ZipFile(epub) as z:
    opf = [n for n in z.namelist() if n.endswith(".opf")][0]
    text = z.read(opf).decode("utf-8", "replace")
    prefix = os.path.dirname(opf)
    manifest = dict(re.findall(r'<item[^>]*\bid="([^"]+)"[^>]*\bhref="([^"]+)"', text))
    manifest.update(dict((m[1], m[0]) for m in re.findall(r'<item[^>]*\bhref="([^"]+)"[^>]*\bid="([^"]+)"', text)))
    idrefs = re.findall(r'<itemref[^>]*idref="([^"]+)"', text)
    index = 0
    for idref in idrefs:
        href = manifest.get(idref)
        if href is None:
            continue
        name = os.path.normpath(os.path.join(prefix, href)) if prefix else href
        if not name.lower().endswith((".xhtml", ".html", ".xml")) or name.endswith((".ncx",)):
            continue
        index += 1
        out = "%s_chapter_%d_%s_%s.wav" % (base, index, voice, name.replace("/", "_"))
        subprocess.run(["ffmpeg", "-v", "error", "-y", "-f", "lavfi",
                        "-i", "sine=frequency=200:duration=%.1f" % (0.5 + index * 0.5),
                        "-ar", "24000", "-ac", "1", out], check=True)
        print("Progress: %d%%" % (index * 100 // max(index, len(idrefs))))
print("Estimated time remaining: 00d 00h 00m 01s")
`
	path := filepath.Join(dir, "audiblez")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_ARGS_FILE", argsFile)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestConvertBookEndToEnd(t *testing.T) {
	requireFFMpeg(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "in")
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := writeFixtureEpub(t, filepath.Join(src, "Fixture Book.epub"), true)
	argsFile := filepath.Join(dir, "args")
	fakeAudiblez(t, argsFile)

	cfg := defaultConfig()
	cfg.FolderIn = src
	cfg.FolderOut = out
	cfg.Voice = "test_voice"
	cfg.Bitrate = "64k"
	cfg.BuildM4B = true

	b := &Book{EpubPath: fixture, BookName: "Fixture Book.epub", SourceFolder: src}
	if err := convertBook(b, cfg); err != nil {
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
	want := []string{"1 - Chapter One.mp3", "2 - Chapter Two.mp3", "Fixture Book.epub", "Fixture Book.m4b"}
	if len(names) != len(want) {
		t.Fatalf("output = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("output %d = %q, want %q", i, names[i], want[i])
		}
	}

	// audiblez has to be called with the epub named "chapters.epub": it derives
	// its wav file names from it, and the wav to chapter mapping depends on that.
	rawArgs, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := string(rawArgs)
	if !strings.Contains(args, "chapters.epub") || !strings.Contains(args, "test_voice") {
		t.Errorf("unexpected audiblez arguments: %q", args)
	}
	if strings.Contains(args, fixture) {
		t.Errorf("audiblez was called with the absolute epub path: %q", args)
	}

	// The m4b has to carry the chapter names, at the right offsets.
	chs := probeChapters(t, filepath.Join(bookDir, "Fixture Book.m4b"))
	if len(chs) != 2 {
		t.Fatalf("m4b has %d chapters: %+v", len(chs), chs)
	}
	if chs[0].Title != "Chapter One" || chs[1].Title != "Chapter Two" {
		t.Errorf("m4b chapter titles = %q, %q", chs[0].Title, chs[1].Title)
	}
	// The fake audio is 1.0s + 1.5s long.
	if d := chs[1].Start; d < 0.9 || d > 1.1 {
		t.Errorf("second chapter starts at %.3fs, want ~1.0s", d)
	}
	total := probeDurationOf(t, filepath.Join(bookDir, "Fixture Book.m4b"))
	if total < 2.3 || total > 2.7 {
		t.Errorf("m4b duration %.3fs, want ~2.5s", total)
	}

	// The source epub is copied as the finished marker.
	if fi, err := os.Stat(filepath.Join(bookDir, "Fixture Book.epub")); err != nil || fi.Size() == 0 {
		t.Errorf("finished epub missing: %v", err)
	}
	// Nothing left behind in the work directory.
	if _, err := os.Stat(filepath.Join(os.TempDir(), "audiobookconverter")); err == nil {
		if leftovers, _ := filepath.Glob(filepath.Join(os.TempDir(), "audiobookconverter", "*", "Fixture Book", "*.wav")); len(leftovers) > 0 {
			t.Errorf("wav files left behind: %v", leftovers)
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
