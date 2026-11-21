package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/viper"
)

const ENV_PREFIX = "CONVERTER"

const (
	defaultBitrate  = "192k"
	defaultMinText  = 1000
	defaultMaxDepth = 3
	defaultMaxTry   = 3
	defaultPollMins = 60

	defaultTTSBin      = "qwen_tts"
	defaultTTSSpeaker  = "ryan"
	defaultTTSQuantize = "int8"
	defaultTTSMaxChars = 600
	defaultTTSThreads  = 0  // 0 = let the engine pick (it probes the machine)
	defaultTTSSeed     = -1 // -1 = engine default: a time-based seed per request
	defaultTTSLanguage = "auto"
	defaultTTSSilence  = 500 // ms of inserted quiet between synthesized segments

	// The engine server loads the model (mmap of ~1.5 GB of weights, plus the
	// tokenizer) before its health check passes; a cold page cache can make
	// that take minutes.
	defaultTTSStartup = 300
)

// defaultSkipPatterns are matched case insensitively against a chapter's title.
// They cover the structural and back matter that must not be narrated; a book
// that names a real chapter after one of these words can be rescued with
// CONVERTER_KEEP_PATTERNS.
var defaultSkipPatterns = []string{
	`^(table of )?contents$`,
	`^toc$`,
	`^(references?|bibliography|works cited|end ?notes|source notes)\b`,
	`^glossary\b`,
	`^index$`,
	`^(copyright|©)\b`,
	`^(colophon|imprimatur)\b`,
	`^(title ?page|half ?title|series page)\b`,
	`^(also by|more by|by the same author)\b`,
	`^acknowledg`,
	`^about the (author|editor|translator|illustrator|photographer)\b`,
	`^(a )?note on\b`,
	`^(praise for|advance praise)\b`,
	`^(advertisement|advertisements|also available|promotion)\b`,
	`^(dedication|epigraph)\b`,
	`^publisher'?s? (note|information)\b`,
	`\bbook series$`,
	`^(errata|corrections)$`,
}

type config struct {
	FolderIn     string
	FolderOut    string
	MaxDepth     int
	Speed        float64
	Bitrate      string
	MinTextChars int
	SkipPatterns []*regexp.Regexp
	KeepPatterns []*regexp.Regexp
	MaxAttempts  int
	PollInterval time.Duration

	// Settings of the local Qwen3-TTS engine (see tts.go). Speed applies as
	// the per-request rate.
	TTSBin       string // path of the qwen_tts executable
	TTSModelDir  string // model directory with config.json + model.safetensors
	TTSModelID   string // Hugging Face repository to download when TTSModelDir is empty
	TTSURL       string // existing engine server; empty = start one from TTSBin
	TTSSpeaker   string // narrator voice, see `qwen_tts --list-speakers`
	TTSQuantize  string // "" = full precision, "int8", "mixed-cpu" (startup flag)
	TTSMaxChars  int    // synthesis budget per engine request, in characters
	TTSThreads   int    // engine threads, 0 = engine default (startup flag)
	TTSSeed      int64  // >= 0: reproducible synthesis; -1: random per request
	TTSLanguage  string // "auto", "German" or "English"
	TTSSilenceMS int    // quiet inserted between engine requests inside a chapter
	// TTSStartup bounds the seconds the engine server gets to load the model
	// before the converter gives up on it.
	TTSStartup int
}

// defaultConfig is the configuration without any environment input. Every
// option's default lives here and only here.
func defaultConfig() config {
	patterns, err := compilePatterns(defaultSkipPatterns)
	if err != nil {
		log.Fatalf("Invalid default skip pattern: %v", err)
	}
	return config{
		MaxDepth:     defaultMaxDepth,
		Speed:        1.0,
		Bitrate:      defaultBitrate,
		MinTextChars: defaultMinText,
		MaxAttempts:  defaultMaxTry,
		PollInterval: time.Duration(defaultPollMins) * time.Minute,
		SkipPatterns: patterns,

		TTSBin:       defaultTTSBin,
		TTSModelDir:  "",
		TTSModelID:   defaultTTSModelID,
		TTSURL:       "",
		TTSSpeaker:   defaultTTSSpeaker,
		TTSQuantize:  defaultTTSQuantize,
		TTSMaxChars:  defaultTTSMaxChars,
		TTSThreads:   defaultTTSThreads,
		TTSSeed:      defaultTTSSeed,
		TTSLanguage:  defaultTTSLanguage,
		TTSSilenceMS: defaultTTSSilence,
		TTSStartup:   defaultTTSStartup,
	}
}

// loadConfig reads the environment and checks the tools we depend on.
func loadConfig() config {
	base := defaultConfig()
	viper.AutomaticEnv()
	viper.SetEnvPrefix(ENV_PREFIX)
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	viper.SetDefault("bitrate", base.Bitrate)
	viper.SetDefault("speed", base.Speed)
	viper.SetDefault("max_depth", base.MaxDepth)
	viper.SetDefault("min_text_chars", base.MinTextChars)
	viper.SetDefault("max_attempts", base.MaxAttempts)
	viper.SetDefault("poll_minutes", int(base.PollInterval.Minutes()))
	viper.SetDefault("tts_bin", base.TTSBin)
	viper.SetDefault("tts_model", base.TTSModelDir)
	viper.SetDefault("tts_model_id", base.TTSModelID)
	viper.SetDefault("tts_url", base.TTSURL)
	viper.SetDefault("tts_startup_seconds", base.TTSStartup)
	viper.SetDefault("tts_speaker", base.TTSSpeaker)
	viper.SetDefault("tts_quantize", base.TTSQuantize)
	viper.SetDefault("tts_max_chars", base.TTSMaxChars)
	viper.SetDefault("tts_threads", base.TTSThreads)
	viper.SetDefault("tts_seed", base.TTSSeed)
	viper.SetDefault("tts_language", base.TTSLanguage)
	viper.SetDefault("tts_silence_ms", base.TTSSilenceMS)

	cfg := config{
		FolderIn:     viper.GetString("in"),
		FolderOut:    viper.GetString("out"),
		MaxDepth:     viper.GetInt("max_depth"),
		Speed:        viper.GetFloat64("speed"),
		Bitrate:      viper.GetString("bitrate"),
		MinTextChars: viper.GetInt("min_text_chars"),
		MaxAttempts:  viper.GetInt("max_attempts"),
		PollInterval: time.Duration(viper.GetInt("poll_minutes")) * time.Minute,

		TTSBin:       viper.GetString("tts_bin"),
		TTSModelDir:  viper.GetString("tts_model"),
		TTSModelID:   viper.GetString("tts_model_id"),
		TTSURL:       viper.GetString("tts_url"),
		TTSSpeaker:   viper.GetString("tts_speaker"),
		TTSQuantize:  strings.ToLower(viper.GetString("tts_quantize")),
		TTSMaxChars:  viper.GetInt("tts_max_chars"),
		TTSThreads:   viper.GetInt("tts_threads"),
		TTSSeed:      viper.GetInt64("tts_seed"),
		TTSLanguage:  viper.GetString("tts_language"),
		TTSSilenceMS: viper.GetInt("tts_silence_ms"),
		TTSStartup:   viper.GetInt("tts_startup_seconds"),
	}
	// FOLDER_IN/FOLDER_OUT were the names used before the CONVERTER_ prefix was
	// applied consistently; keep accepting them.
	if cfg.FolderIn == "" {
		cfg.FolderIn = os.Getenv("FOLDER_IN")
	}
	if cfg.FolderOut == "" {
		cfg.FolderOut = os.Getenv("FOLDER_OUT")
	}
	// No model folder given: models live in the user cache, below a name that
	// tells the repositories apart.
	if cfg.TTSModelDir == "" {
		dir, err := modelCacheDir(cfg.TTSModelID)
		if err != nil {
			log.Fatalf("Error: could not locate a user cache folder: %v", err)
		}
		cfg.TTSModelDir = dir
	}
	if cfg.Speed < 0.5 || cfg.Speed > 2.0 {
		log.Printf("Ignoring speed %v, must be between 0.5 and 2.0", cfg.Speed)
		cfg.Speed = 1.0
	}
	// 1792 characters is the request limit of a stock engine server; a bigger
	// budget would have to be raised on the server itself.
	if cfg.TTSMaxChars < 120 || cfg.TTSMaxChars > 1792 {
		log.Printf("Ignoring %s_TTS_MAX_CHARS %d, must be between 120 and 1792",
			ENV_PREFIX, cfg.TTSMaxChars)
		cfg.TTSMaxChars = base.TTSMaxChars
	}
	if cfg.TTSStartup <= 0 {
		cfg.TTSStartup = base.TTSStartup
	}
	if cfg.TTSSilenceMS < 0 || cfg.TTSSilenceMS > 3000 {
		log.Printf("Ignoring %s_TTS_SILENCE_MS %d, must be between 0 and 3000",
			ENV_PREFIX, cfg.TTSSilenceMS)
		cfg.TTSSilenceMS = base.TTSSilenceMS
	}
	if cfg.TTSThreads < 0 {
		cfg.TTSThreads = base.TTSThreads
	}

	patterns := defaultSkipPatterns
	if extra := viper.GetString("extra_skip_patterns"); extra != "" {
		patterns = append(patterns, splitPatterns(extra)...)
	}
	var err error
	if cfg.SkipPatterns, err = compilePatterns(patterns); err != nil {
		log.Fatalf("Invalid skip pattern: %v", err)
	}
	if cfg.KeepPatterns, err = compilePatterns(splitPatterns(viper.GetString("keep_patterns"))); err != nil {
		log.Fatalf("Invalid keep pattern: %v", err)
	}

	log.Printf("%s_IN: %s", ENV_PREFIX, cfg.FolderIn)
	log.Printf("%s_OUT: %s", ENV_PREFIX, cfg.FolderOut)
	log.Printf("%s_BITRATE: %s, %s_SPEED: %v", ENV_PREFIX, cfg.Bitrate, ENV_PREFIX, cfg.Speed)
	log.Printf("%s_MIN_TEXT_CHARS: %d, %s_POLL_MINUTES: %v, %s_MAX_ATTEMPTS: %d",
		ENV_PREFIX, cfg.MinTextChars, ENV_PREFIX, cfg.PollInterval, ENV_PREFIX, cfg.MaxAttempts)
	log.Printf("%s_TTS_SPEAKER: %s, %s_TTS_MAX_CHARS: %d, %s_TTS_LANGUAGE: %s",
		ENV_PREFIX, cfg.TTSSpeaker, ENV_PREFIX, cfg.TTSMaxChars, ENV_PREFIX, cfg.TTSLanguage)
	log.Printf("%s_TTS_SEED: %d, %s_TTS_SILENCE_MS: %d",
		ENV_PREFIX, cfg.TTSSeed, ENV_PREFIX, cfg.TTSSilenceMS)
	if cfg.TTSURL != "" {
		log.Printf("%s_TTS_URL: %s (using an engine server that is already running)",
			ENV_PREFIX, cfg.TTSURL)
	} else {
		log.Printf("%s_TTS_MODEL: %s, %s_TTS_QUANTIZE: %q, %s_TTS_THREADS: %d",
			ENV_PREFIX, cfg.TTSModelDir, ENV_PREFIX, cfg.TTSQuantize, ENV_PREFIX, cfg.TTSThreads)
	}

	if cfg.FolderIn == "" || cfg.FolderOut == "" {
		log.Fatalf("Error: %s_IN and %s_OUT must both be set", ENV_PREFIX, ENV_PREFIX)
	}
	if cfg.TTSURL != "" && !strings.HasPrefix(cfg.TTSURL, "http://") &&
		!strings.HasPrefix(cfg.TTSURL, "https://") {
		log.Fatalf("Error: %s_TTS_URL must be empty or an http(s) URL, got %q", ENV_PREFIX, cfg.TTSURL)
	}
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			log.Fatalf("Error: could not find the '%s' executable: %v", tool, err)
		}
	}
	// The engine is only needed when we start it ourselves; an external
	// server's binary and model live on that machine.
	if cfg.TTSURL == "" {
		// An absolute or ./-relative binary is checked as given; a bare name
		// must resolve through PATH.
		if _, err := exec.LookPath(cfg.TTSBin); err != nil {
			log.Fatalf("Error: could not find the '%s' executable: %v", cfg.TTSBin, err)
		}
		switch cfg.TTSQuantize {
		case "", "int8", "mixed-cpu":
		default:
			log.Fatalf("Error: %s_TTS_QUANTIZE must be empty, 'int8' or 'mixed-cpu', got %q",
				ENV_PREFIX, cfg.TTSQuantize)
		}
	}
	if err := os.MkdirAll(cfg.FolderOut, 0o755); err != nil {
		log.Fatalf("Error: could not create %s_OUT: %v", ENV_PREFIX, err)
	}
	// Fetch the model when it is not there yet - after the cheap checks, so
	// that a misconfigured folder does not cost a gigabyte download first.
	if cfg.TTSURL == "" {
		if err := ensureModel(cfg.TTSModelDir, cfg.TTSModelID); err != nil {
			log.Fatalf("Error: model is not available: %v\n"+
				"  Point %s_TTS_MODEL at a downloaded model directory, set\n"+
				"  %s_TTS_MODEL_ID to the repository to fetch, or %s_TTS_URL at a\n"+
				"  running engine server.",
				err, ENV_PREFIX, ENV_PREFIX, ENV_PREFIX)
		}
	}
	log.Println("All checks passed. Starting...")
	return cfg
}

// splitPatterns splits a comma separated option list, allowing "\," to escape a
// literal comma inside a regular expression.
func splitPatterns(value string) []string {
	var out []string
	var cur strings.Builder
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '\\':
			if i+1 < len(value) && value[i+1] == ',' {
				i++
				cur.WriteByte(',')
				continue
			}
			cur.WriteByte(value[i])
		case ',':
			if s := strings.TrimSpace(cur.String()); s != "" {
				out = append(out, s)
			}
			cur.Reset()
		default:
			cur.WriteByte(value[i])
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}

func compilePatterns(patterns []string) ([]*regexp.Regexp, error) {
	var out []*regexp.Regexp
	for _, p := range patterns {
		re, err := regexp.Compile(`(?i)` + p)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", p, err)
		}
		out = append(out, re)
	}
	return out, nil
}
