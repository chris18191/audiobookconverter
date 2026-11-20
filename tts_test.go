package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// TestMain lets the test binary double as the engine server: the fake child
// process that ttsEngine spawns is the test binary itself, re-executed with
// "fake-engine" as the first argument (see fakeEngineBin).
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "fake-engine" {
		runFakeEngineChild()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestSplitSentences(t *testing.T) {
	tests := []struct {
		name  string
		lang  string
		input string
		want  []string
	}{
		{
			name:  "plain english",
			lang:  langEnglish,
			input: "The light burned. Emma slept.",
			want:  []string{"The light burned.", "Emma slept."},
		},
		{
			name:  "german dialogue stays with its frame",
			lang:  langGerman,
			input: "Emma kam. „Bist du da?“, fragte sie. Winter nickte.",
			want:  []string{"Emma kam.", "„Bist du da?“, fragte sie.", "Winter nickte."},
		},
		{
			name:  "titles and abbreviations",
			input: "Dr. Winter fuhr nach z. B. St. Georgen. Spät.",
			want:  []string{"Dr. Winter fuhr nach z. B. St. Georgen.", "Spät."},
		},
		{
			name:  "initials do not end a sentence",
			input: "Sie hieß J. K. Rowling. Später ging sie.",
			want:  []string{"Sie hieß J. K. Rowling.", "Später ging sie."},
		},
		{
			name:  "decimals",
			input: "Der Wert ist 3.14. Genau.",
			want:  []string{"Der Wert ist 3.14.", "Genau."},
		},
		{
			name:  "ellipsis and exclamation",
			input: "Er ging … Plötzlich Stille! Dann nichts.",
			want:  []string{"Er ging …", "Plötzlich Stille!", "Dann nichts."},
		},
		{
			name:  "lowercase continuation is no boundary",
			input: "„Wach auf“, murmelte er und schlief weiter.",
			want:  []string{"„Wach auf“, murmelte er und schlief weiter."},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := splitSentences(tc.input, tc.lang)
			if len(got) != len(tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("sentence %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestSegmentsForTextBudgetAndShape(t *testing.T) {
	text := "Das Licht brannte die ganze Nacht. Emma wachte am Fenster. „Bist du da?“, fragte sie leise in den Sturm hinein, und der Wind trug ihre Worte fort. Der alte Turm schwieg.\n" +
		"Am Morgen war alles ruhig.\n" +
		"* * *\n" +
		"Die Fischer kamen zurück."
	segments := segmentsForText(text, 80, langGerman)
	if len(segments) < 3 {
		t.Fatalf("expected the text to be split, got %q", segments)
	}
	for _, seg := range segments {
		if utf8Len(seg) > 80 {
			t.Errorf("segment over budget (%d): %q", utf8Len(seg), seg)
		}
		if !speakable(seg) {
			t.Errorf("unspeakable segment survived: %q", seg)
		}
		if strings.Contains(seg, "\n") {
			t.Errorf("segment kept a newline: %q", seg)
		}
	}
	// The dialogue must stay whole inside one segment.
	if !strings.Contains(strings.Join(segments, "\n"), "„Bist du da?“, fragte sie") {
		t.Errorf("dialogue torn apart: %q", segments)
	}
	// Paragraphs are never glued together: no segment spans the paragraph break.
	joined := strings.Join(segments, "\n")
	if strings.Contains(joined, "Der alte Turm schwieg. Am Morgen") {
		t.Error("paragraphs were merged into one breath")
	}
}

func TestSplitOversizedKeepsQuotationTogether(t *testing.T) {
	sentence := "Sie rief: „Es war ein langer Weg bis zum Turm und wieder zurück.“ Dann wurde es still."
	pieces := splitOversized(sentence, 40)
	for _, p := range pieces {
		if quoteBalance([]rune(p)) > 0 && !strings.HasPrefix(p, "„") {
			t.Errorf("a quotation was torn open: %q", p)
		}
	}
	// The narrator lead-in must not be abandoned inside the dialogue.
	if pieces[0] != "Sie rief:" {
		t.Errorf("first piece = %q, wanted the lead-in before the quote", pieces[0])
	}
}

// --- fake engine -----------------------------------------------------------

// fakeEngine is a stand-in for the qwen3-tts HTTP server: it answers /v1/health
// and /v1/tts with deterministic sine wavs and records every request. It runs
// either inside the test process (httptest, for the external-server mode) or
// inside a re-executed child (for the child-server mode).
type fakeEngine struct {
	mu        sync.Mutex
	requests  []ttsRequest
	served    int
	argsFile  string // one request JSON per line, needed across process borders
	noAudio   bool   // answer with a body too short to be audio
	server500 bool   // answer /v1/tts with an engine-side error
	stopAfter int    // serve short bodies beyond this many requests
}

func (f *fakeEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/health":
		fmt.Fprint(w, "ok")
	case "/v1/tts":
		var req ttsRequest
		raw, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.served++
		n := f.served
		if json.Unmarshal(raw, &req) != nil {
			req = ttsRequest{Text: string(raw)}
		}
		f.requests = append(f.requests, req)
		if f.argsFile != "" {
			fh, ferr := os.OpenFile(f.argsFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if ferr == nil {
				fmt.Fprintf(fh, "%s\n", raw)
				fh.Close()
			}
		}
		f.mu.Unlock()
		switch {
		case f.server500:
			http.Error(w, "CUDA out of memory", http.StatusInternalServerError)
		case f.noAudio, f.stopAfter > 0 && n > f.stopAfter:
			fmt.Fprint(w, "<not audio>")
		default:
			w.Header().Set("Content-Type", "audio/wav")
			w.Write(testWav(1.0))
		}
	default:
		http.NotFound(w, r)
	}
}

// requestLog returns the recorded requests (external-server mode).
func (f *fakeEngine) requestLog() []ttsRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ttsRequest(nil), f.requests...)
}

// servedCount returns how many /v1/tts requests were answered.
func (f *fakeEngine) servedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.served
}

// fakeArgs reads back the request log of the child engine.
func fakeArgs(t *testing.T, argsFile string) []ttsRequest {
	t.Helper()
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("engine request log: %v", err)
	}
	var out []ttsRequest
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var req ttsRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			t.Fatalf("engine request log line %q: %v", line, err)
		}
		out = append(out, req)
	}
	return out
}

// newFakeEngine starts the fake inside the test process.
func newFakeEngine(t *testing.T) (*fakeEngine, *httptest.Server) {
	t.Helper()
	f := &fakeEngine{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

// fakeEngineBin writes an executable that re-runs the test binary as the fake
// engine server, so the child lifecycle (spawn, health polling, death, restart)
// is tested against a real process without needing the model.
func fakeEngineBin(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "qwen3-tts")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec "+exe+" fake-engine \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// runFakeEngineChild is the entry point of the re-executed test binary. The
// environment selects its failure modes; port and flags arrive like they do
// for the real engine (see startLocked).
func runFakeEngineChild() {
	args := os.Args[2:]
	port := ""
	for i, a := range args {
		if a == "--serve" && i+1 < len(args) {
			port = args[i+1]
		}
	}
	if os.Getenv("FAKE_ENGINE_FAIL") != "" {
		fmt.Fprintln(os.Stderr, "model load failed: boom")
		os.Exit(3)
	}
	stop, _ := strconv.Atoi(os.Getenv("FAKE_STOP_AFTER"))
	f := &fakeEngine{
		argsFile:  os.Getenv("FAKE_ARGS_FILE"),
		noAudio:   os.Getenv("FAKE_NOAUDIO") != "",
		stopAfter: stop,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake engine cannot listen: %v\n", err)
		os.Exit(2)
	}
	_ = http.Serve(ln, f)
}

// testWav renders a mono 24 kHz pcm_s16le sine wav of the given length.
func testWav(seconds float64) []byte {
	const rate = 24000
	n := int(float64(rate) * seconds)
	buf := make([]byte, 44+n*2)
	copy(buf[0:], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:], uint32(36+n*2))
	copy(buf[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(buf[16:], 16)
	binary.LittleEndian.PutUint16(buf[20:], 1) // PCM
	binary.LittleEndian.PutUint16(buf[22:], 1) // mono
	binary.LittleEndian.PutUint32(buf[24:], rate)
	binary.LittleEndian.PutUint32(buf[28:], rate*2)
	binary.LittleEndian.PutUint16(buf[32:], 2)
	binary.LittleEndian.PutUint16(buf[34:], 16)
	copy(buf[36:], "data")
	binary.LittleEndian.PutUint32(buf[40:], uint32(n*2))
	for i := 0; i < n; i++ {
		v := int16(math.Sin(2*math.Pi*440*float64(i)/rate) * 12000)
		binary.LittleEndian.PutUint16(buf[44+i*2:], uint16(v))
	}
	return buf
}

// --- engine tests ----------------------------------------------------------

// engineChapters are two short chapters; with a generous segment budget each
// becomes exactly one engine request.
func engineChapters() []epubChapter {
	return []epubChapter{
		{Title: "Kapitel Eins", Text: "Der Leuchtturm stand vor dem Dorf und brannte jede Nacht. Emma hatte ihn geerbt, samt dem Logbuch ihres Großvaters."},
		{Title: "Kapitel Zwei", Text: "Der Sturm kam drei Wochen später."},
	}
}

func engineConfig(t *testing.T) config {
	t.Helper()
	cfg := defaultConfig()
	cfg.TTSModelDir = t.TempDir()
	cfg.TTSSpeaker = "test_speaker"
	cfg.TTSLanguage = "English"
	cfg.TTSMaxChars = 4000 // one segment per chapter unless the test wants more
	return cfg
}

func engineFixtureEpub(t *testing.T) *epubBook {
	t.Helper()
	path := writeFixtureEpub(t, filepath.Join(t.TempDir(), "Fix.epub"), true)
	b, err := openEpub(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEngineChildSynthesizesChapters(t *testing.T) {
	requireFFMpeg(t)
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(dir, "engine-args.txt")
	t.Setenv("FAKE_ARGS_FILE", argsFile)

	cfg := engineConfig(t)
	cfg.TTSBin = fakeEngineBin(t)
	eng := newTTSEngine(cfg)
	defer eng.stop()

	audio, err := eng.synthesize(work, engineFixtureEpub(t), engineChapters(), &conversionStatus{})
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if len(audio) != 2 {
		t.Fatalf("audio = %+v, want 2 chapters", audio)
	}
	want := []struct {
		file     string
		title    string
		duration float64
	}{
		{"chapter_001.wav", "Kapitel Eins", 1.0},
		{"chapter_002.wav", "Kapitel Zwei", 1.0},
	}
	for i, w := range want {
		if audio[i].File != w.file || audio[i].Title != w.title {
			t.Errorf("audio %d = %q %q, want %q %q", i, audio[i].File, audio[i].Title, w.file, w.title)
		}
		if d := audio[i].Duration; d < w.duration-0.05 || d > w.duration+0.05 {
			t.Errorf("audio %d duration = %.3f, want %.1f", i, d, w.duration)
		}
		if fi, err := os.Stat(filepath.Join(work, w.file)); err != nil || fi.Size() < 40000 {
			t.Errorf("chapter file %s missing or tiny: %v", w.file, err)
		}
	}

	// Every request carries the configured speaker, the book language, and the
	// text of exactly one segment; engine-reset defaults stay unsent.
	reqs := fakeArgs(t, argsFile)
	if len(reqs) != 2 {
		t.Fatalf("engine saw %d requests, want 2", len(reqs))
	}
	for i, req := range reqs {
		if req.Speaker != "test_speaker" || req.Language != "English" {
			t.Errorf("request %d = %+v", i, req)
		}
		if !strings.HasPrefix(reqs[0].Text, "Der Leuchtturm") && !strings.HasPrefix(reqs[i].Text, "Der Sturm") {
			t.Errorf("request %d text = %q", i, req.Text)
		}
	}
	if raw, err := os.ReadFile(argsFile); err == nil &&
		(strings.Contains(string(raw), `"rate"`) || strings.Contains(string(raw), `"seed"`)) {
		t.Errorf("defaults were sent to the engine: %s", raw)
	}
}

func TestEngineReportsProgress(t *testing.T) {
	requireFFMpeg(t)
	work := t.TempDir()
	t.Setenv("FAKE_ARGS_FILE", filepath.Join(t.TempDir(), "args.txt"))
	cfg := engineConfig(t)
	cfg.TTSBin = fakeEngineBin(t)
	cfg.TTSMaxChars = 12 // forces several segments per chapter
	eng := newTTSEngine(cfg)
	defer eng.stop()

	status := &conversionStatus{}
	if _, err := eng.synthesize(work, engineFixtureEpub(t), engineChapters(), status); err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if percent, _ := status.snapshot(); percent != 100 {
		t.Errorf("progress after a finished synthesis = %d%%, want 100%%", percent)
	}
}

func TestEngineSilenceBetweenSegments(t *testing.T) {
	requireFFMpeg(t)
	work := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	t.Setenv("FAKE_ARGS_FILE", argsFile)
	cfg := engineConfig(t)
	cfg.TTSBin = fakeEngineBin(t)
	cfg.TTSMaxChars = 12 // the chapter becomes several one-second segments
	cfg.TTSSilenceMS = 60
	cfg.Speed = 1.25
	cfg.TTSSeed = 7
	eng := newTTSEngine(cfg)
	defer eng.stop()

	audio, err := eng.synthesize(work, engineFixtureEpub(t), engineChapters()[:1], &conversionStatus{})
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	reqs := fakeArgs(t, argsFile)
	if len(reqs) < 2 {
		t.Fatalf("expected several segments, got %d", len(reqs))
	}
	// The per-request knobs arrive at the server.
	for _, req := range reqs {
		if req.Rate == nil || *req.Rate != 1.25 || req.Seed == nil || *req.Seed != 7 {
			t.Errorf("request knobs lost: %+v", req)
		}
	}
	// n one-second segments plus (n-1) 60ms pauses.
	want := float64(len(reqs)) + float64(len(reqs)-1)*0.06
	if d := audio[0].Duration; d < want-0.15 || d > want+0.15 {
		t.Errorf("assembled chapter is %.3fs, want %.3fs (segments + pauses)", d, want)
	}
}

func TestEngineChildDiesBetweenSegments(t *testing.T) {
	requireFFMpeg(t)
	work := t.TempDir()
	t.Setenv("FAKE_ARGS_FILE", filepath.Join(t.TempDir(), "args.txt"))
	cfg := engineConfig(t)
	cfg.TTSBin = fakeEngineBin(t)
	eng := newTTSEngine(cfg)
	defer eng.stop()

	if err := eng.ensure(); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	first := eng.cmd // recorded for the restart assertion
	if err := first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	// The connection dies; synthesizeSegment must restart the server and
	// retry the segment instead of failing the book.
	if err := eng.synthesizeSegment("Der Sturm kam.", filepath.Join(work, "seg.wav"), langEnglish); err != nil {
		t.Fatalf("segment after child death: %v", err)
	}
	if eng.cmd == first {
		t.Error("the dead child was not replaced")
	}
	if fi, err := os.Stat(filepath.Join(work, "seg.wav")); err != nil || fi.Size() < 40000 {
		t.Errorf("retried segment missing: %v", err)
	}
}

func TestEngineStartupFailure(t *testing.T) {
	t.Setenv("FAKE_ENGINE_FAIL", "1")
	cfg := engineConfig(t)
	cfg.TTSBin = fakeEngineBin(t)
	cfg.TTSStartup = 10
	eng := newTTSEngine(cfg)
	defer eng.stop()

	err := eng.ensure()
	if err == nil {
		t.Fatal("an engine that dies at startup was accepted")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("the engine's own error did not reach the message: %v", err)
	}
}

func TestEngineExternalNoSpawn(t *testing.T) {
	requireFFMpeg(t)
	fake, srv := newFakeEngine(t)
	work := t.TempDir()
	cfg := engineConfig(t)
	cfg.TTSURL = srv.URL
	cfg.TTSBin = "/this/binary/must/not/be/started"
	eng := newTTSEngine(cfg)
	defer eng.stop()

	audio, err := eng.synthesize(work, engineFixtureEpub(t), engineChapters(), &conversionStatus{})
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if len(audio) != 2 {
		t.Fatalf("audio = %+v", audio)
	}
	if eng.cmd != nil {
		t.Error("an external engine was spawned as a child anyway")
	}
	if n := fake.servedCount(); n != 2 {
		t.Errorf("external engine saw %d requests, want 2", n)
	}
	if reqs := fake.requestLog(); len(reqs) == 0 || reqs[0].Speaker != "test_speaker" {
		t.Errorf("requests = %+v", reqs)
	}
}

func TestEngineEmptyAudioFailsTheBook(t *testing.T) {
	fake, srv := newFakeEngine(t)
	fake.noAudio = true
	cfg := engineConfig(t)
	cfg.TTSURL = srv.URL
	eng := newTTSEngine(cfg)
	defer eng.stop()

	_, err := eng.synthesize(t.TempDir(), engineFixtureEpub(t), engineChapters(), &conversionStatus{})
	if err == nil {
		t.Fatal("a body without audio was accepted")
	}
	if !strings.Contains(err.Error(), "bytes of audio") {
		t.Errorf("error = %v", err)
	}
}

func TestEngineServerErrorStops(t *testing.T) {
	fake, srv := newFakeEngine(t)
	fake.server500 = true
	cfg := engineConfig(t)
	cfg.TTSURL = srv.URL
	eng := newTTSEngine(cfg)
	defer eng.stop()

	_, err := eng.synthesize(t.TempDir(), engineFixtureEpub(t), engineChapters(), &conversionStatus{})
	if err == nil {
		t.Fatal("an engine-side 500 was accepted")
	}
	if !strings.Contains(err.Error(), "CUDA out of memory") {
		t.Errorf("the engine's answer did not reach the error: %v", err)
	}
	// A 5xx comes from a live server: retrying the request burst would only
	// multiply the load.
	if n := fake.servedCount(); n != 1 {
		t.Errorf("the failed request was retried %d times", n)
	}
}

func TestEngineUnreachableExternalFailsFast(t *testing.T) {
	// Port 1 is reserved: connecting answers instantly with ECONNREFUSED, no
	// startup deadline has to expire first.
	cfg := engineConfig(t)
	cfg.TTSURL = "http://127.0.0.1:1/"
	eng := newTTSEngine(cfg)
	defer eng.stop()

	_, err := eng.synthesize(t.TempDir(), engineFixtureEpub(t), engineChapters(), &conversionStatus{})
	if err == nil {
		t.Fatal("an unreachable engine was accepted")
	}
}
