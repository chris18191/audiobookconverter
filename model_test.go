package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeHF serves a model repository: every modelFiles entry, with the big file
// answerable in ranges, mirroring what Hugging Face resolve URLs do.
func fakeHF(t *testing.T, sizes map[string]int) *httptest.Server {
	t.Helper()
	body := func(name string) string {
		if n, ok := sizes[name]; ok {
			return strings.Repeat("x", n)
		}
		return `{"ok": "` + name + `"}`
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		const pre = "/Qwen/Qwen3-TTS-12Hz-0.6B-CustomVoice/resolve/main/"
		if !strings.HasPrefix(r.URL.Path, pre) {
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, pre)
		if !strings.Contains(name, "speech_tokenizer") && !knownModelFile(name) {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, name, time.Time{}, strings.NewReader(body(name)))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func knownModelFile(name string) bool {
	for _, f := range modelFiles {
		if f == name {
			return true
		}
	}
	return false
}

func TestEnsureModelDownloadsEverything(t *testing.T) {
	srv := fakeHF(t, nil)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	old := modelBaseURL
	modelBaseURL = srv.URL
	defer func() { modelBaseURL = old }()

	dir := filepath.Join(t.TempDir(), "model")
	if err := ensureModel(dir, defaultTTSModelID); err != nil {
		t.Fatalf("ensureModel: %v", err)
	}
	for _, f := range modelFiles {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); err != nil {
			t.Errorf("%s missing after download: %v", f, err)
		}
	}
	// No .part leftovers.
	leftovers, _ := filepath.Glob(filepath.Join(dir, "*.part"))
	if len(leftovers) > 0 {
		t.Errorf("leftover partial files: %v", leftovers)
	}
}

// A model directory that already has config.json must not touch the network.
func TestEnsureModelKeepsExisting(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(500)
	}))
	defer srv.Close()
	old := modelBaseURL
	modelBaseURL = srv.URL
	defer func() { modelBaseURL = old }()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureModel(dir, defaultTTSModelID); err != nil {
		t.Fatalf("ensureModel: %v", err)
	}
	if calls != 0 {
		t.Errorf("existing model re-downloaded: %d requests", calls)
	}
}

// An interrupted .part continues with a Range request instead of starting
// over; the test server answers ranges so only the missing tail is written.
func TestEnsureModelResumesPartialFile(t *testing.T) {
	const content = "the quick brown fox"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "model.safetensors", time.Time{}, strings.NewReader(content))
	}))
	defer srv.Close()
	old := modelBaseURL
	modelBaseURL = srv.URL
	defer func() { modelBaseURL = old }()

	dir := t.TempDir()
	part := filepath.Join(dir, "model.safetensors.part")
	if err := os.WriteFile(part, []byte("the quick "), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := downloadModelFile(defaultTTSModelID, "model.safetensors", part[:len(part)-len(".part")]); err != nil {
		t.Fatalf("downloadModelFile: %v", err)
	}
	raw, err := os.ReadFile(strings.TrimSuffix(part, ".part"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != content {
		t.Errorf("resumed file = %q, want %q", raw, content)
	}
}

func TestNormalizeModelID(t *testing.T) {
	if id, err := normalizeModelID("0.6B-CustomVoice"); err != nil || id != defaultTTSModelID {
		t.Errorf("alias = %q, %v; want %q, nil", id, err, defaultTTSModelID)
	}
	if id, err := normalizeModelID(defaultTTSModelID); err != nil || id != defaultTTSModelID {
		t.Errorf("full id = %q, %v", id, err)
	}
	if _, err := normalizeModelID("Qwen/Qwen3-TTS-12Hz-99B-Nonsense"); err == nil {
		t.Error("unknown repository accepted")
	}
}

func TestModelCacheDirSeparatesModels(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/tmp/cache")
	a, err := modelCacheDir("Qwen/Qwen3-TTS-12Hz-0.6B-CustomVoice")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := modelCacheDir("Qwen/Qwen3-TTS-12Hz-1.7B-CustomVoice")
	if a == b {
		t.Errorf("two models share the cache directory %q", a)
	}
	if filepath.Base(a) != "qwen3-tts-12hz-0.6b-customvoice" {
		t.Errorf("cache dir name = %q", filepath.Base(a))
	}
}

// loadConfig must not pay for a download just to answer --help style checks;
// with an external server URL the model folder is irrelevant.
func TestEnsureModelSizeHint(t *testing.T) {
	if !strings.Contains(modelSizeHint("Qwen/Qwen3-TTS-12Hz-1.7B-Base"), "4") ||
		!strings.Contains(modelSizeHint(defaultTTSModelID), "1.1") {
		t.Error("size hints must name the model tier")
	}
}
