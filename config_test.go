package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfigValues(t *testing.T) {
	cfg := defaultConfig()
	if cfg.TTSBin == "" || cfg.TTSSpeaker == "" {
		t.Errorf("qwen defaults missing: %+v", cfg)
	}
	if cfg.TTSURL != "" {
		t.Errorf("default engine URL = %q, want empty (spawn our own)", cfg.TTSURL)
	}
	if cfg.TTSSeed != -1 {
		t.Errorf("default seed = %d, want -1 (engine picks)", cfg.TTSSeed)
	}
	if cfg.TTSSilenceMS <= 0 || cfg.TTSMaxChars < 120 || cfg.TTSMaxChars > 1792 {
		t.Errorf("qwen synthesis defaults out of range: %+v", cfg)
	}
	if cfg.TTSStartup <= 0 {
		t.Errorf("default startup budget = %d, want > 0", cfg.TTSStartup)
	}
	if cfg.TTSLanguage != "auto" {
		t.Errorf("default language = %q, want auto", cfg.TTSLanguage)
	}
}

// qwen-ready environment: folders, a stub binary and a model directory that
// pass loadConfig's checks.
func qwenEnv(t *testing.T) map[string]string {
	t.Helper()
	requireFFMpeg(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "qwen3-tts")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(dir, "model")
	if err := os.MkdirAll(model, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(model, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"CONVERTER_IN":             filepath.Join(dir, "in"),
		"CONVERTER_OUT":            filepath.Join(dir, "out"),
		"CONVERTER_TTS_BIN":        bin,
		"CONVERTER_TTS_MODEL":      model,
		"CONVERTER_TTS_SPEAKER":    "vivian",
		"CONVERTER_TTS_QUANTIZE":   "",
		"CONVERTER_TTS_MAX_CHARS":  "800",
		"CONVERTER_TTS_SEED":       "5",
		"CONVERTER_TTS_SILENCE_MS": "300",
		"CONVERTER_TTS_THREADS":    "3",
		"CONVERTER_TTS_LANGUAGE":   "German",
	}
}

func TestLoadConfigQwenOverrides(t *testing.T) {
	for k, v := range qwenEnv(t) {
		t.Setenv(k, v)
	}
	cfg := loadConfig()
	if cfg.TTSSpeaker != "vivian" || cfg.TTSMaxChars != 800 || cfg.TTSSeed != 5 ||
		cfg.TTSSilenceMS != 300 || cfg.TTSThreads != 3 || cfg.TTSLanguage != "German" {
		t.Errorf("qwen options not picked up: %+v", cfg)
	}
}

// With CONVERTER_TTS_URL the converter does not start a server of its own and
// must not demand a local binary or model directory.
func TestLoadConfigExternalServer(t *testing.T) {
	env := qwenEnv(t)
	for k, v := range env {
		t.Setenv(k, v)
	}
	t.Setenv("CONVERTER_TTS_BIN", filepath.Join(t.TempDir(), "not-installed"))
	t.Setenv("CONVERTER_TTS_MODEL", filepath.Join(t.TempDir(), "no-model"))
	t.Setenv("CONVERTER_TTS_URL", "http://127.0.0.1:8080/")
	cfg := loadConfig()
	if cfg.TTSURL != "http://127.0.0.1:8080/" {
		t.Errorf("engine URL = %q", cfg.TTSURL)
	}
}

// The pre-prefix folder variables keep working.
func TestLoadConfigLegacyFolders(t *testing.T) {
	env := qwenEnv(t)
	in, out := env["CONVERTER_IN"], env["CONVERTER_OUT"]
	for k, v := range env {
		t.Setenv(k, v)
	}
	t.Setenv("CONVERTER_IN", "")
	t.Setenv("CONVERTER_OUT", "")
	t.Setenv("FOLDER_IN", in)
	t.Setenv("FOLDER_OUT", out)
	cfg := loadConfig()
	if cfg.FolderIn != in || cfg.FolderOut != out {
		t.Errorf("legacy folder variables ignored: %+v", cfg)
	}
}

func TestLoadConfigSpeedSanity(t *testing.T) {
	for k, v := range qwenEnv(t) {
		t.Setenv(k, v)
	}
	t.Setenv("CONVERTER_SPEED", "9")
	t.Setenv("CONVERTER_TTS_MAX_CHARS", "10")
	t.Setenv("CONVERTER_TTS_SILENCE_MS", "99999")
	cfg := loadConfig()
	if cfg.Speed != 1.0 {
		t.Errorf("out-of-range speed = %v, want reset to 1.0", cfg.Speed)
	}
	if cfg.TTSMaxChars != defaultConfig().TTSMaxChars {
		t.Errorf("out-of-range max_chars = %d, want reset", cfg.TTSMaxChars)
	}
	if cfg.TTSSilenceMS != defaultConfig().TTSSilenceMS {
		t.Errorf("out-of-range silence = %d, want reset", cfg.TTSSilenceMS)
	}
}
