package main

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/viper"
)

type ProcessingState int

const (
	Error ProcessingState = iota
	New
	InProcess
	Done
)

func (ps ProcessingState) String() string {
	return map[ProcessingState]string{
		Error:     "Failed",
		New:       "Not yet processed",
		InProcess: "Currently processing",
		Done:      "Finished",
	}[ps]
}

type Book struct {
	EpubPath      string
	BookName      string
	SourceFolder  string
	TargetFolder  string
	Status        ProcessingState
	Progress      int
	TimeRemaining string
	Chapters      int
	Attempts      int
	LastError     string
}

const (
	ENV_PREFIX = "CONVERTER"

	defaultVoice    = "bm_lewis"
	defaultBitrate  = "192k"
	defaultMinText  = 1000
	defaultMaxDepth = 3
	defaultMaxTry   = 3
	defaultPollMins = 60
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
	Voice        string
	Speed        float64
	Bitrate      string
	MinTextChars int
	SkipPatterns []*regexp.Regexp
	KeepPatterns []*regexp.Regexp
	BuildM4B     bool
	MaxAttempts  int
	PollInterval time.Duration
}

func main() {
	cfg := loadConfig()
	books := make(map[string]*Book)

	for {
		// Books in the import folder are candidates; a book is considered
		// finished once its epub has been copied next to the audio in the output
		// folder (that copy is what marks it as done).
		scanFolder(books, cfg, cfg.FolderIn, true)
		scanFolder(books, cfg, cfg.FolderOut, false)
		printBooks(books)
		processBooks(books, cfg)

		log.Printf("Waiting %s for new files...", cfg.PollInterval)
		time.Sleep(cfg.PollInterval)
	}
}

// processBooks converts every book that has not been converted yet, one after
// another: audiblez is CPU bound and one book at a time keeps the log readable.
func processBooks(books map[string]*Book, cfg config) {
	for name, book := range books {
		if book.Status != New {
			continue
		}
		if cfg.MaxAttempts > 0 && book.Attempts >= cfg.MaxAttempts {
			log.Printf("Skipping '%s': already failed %d times (last error: %s)", name, book.Attempts, book.LastError)
			continue
		}
		book.Attempts++
		book.Status = InProcess
		log.Printf("Processing book '%s' (attempt %d)...", name, book.Attempts)
		if err := convertBook(book, cfg); err != nil {
			book.Status = Error
			book.LastError = err.Error()
			log.Printf("Book '%s' failed: %v", name, err)
			continue
		}
		book.Status = Done
		book.Progress = 100
		book.TimeRemaining = ""
		log.Printf("Finished book '%s'", name)
	}
}

// convertBook selects the chapters, runs audiblez on a derived epub holding only
// those chapters and writes title named audio to the output folder.
func convertBook(book *Book, cfg config) error {
	bookFolder := filepath.Join(cfg.FolderOut, strings.TrimSuffix(book.BookName, filepath.Ext(book.BookName)))

	workDir := filepath.Join(os.TempDir(), "audiobookconverter",
		time.Now().Format("2006-01-02_15_04_05"), sanitizeFileName(book.BookName))
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return fmt.Errorf("could not create work directory %s: %w", workDir, err)
	}
	defer os.RemoveAll(filepath.Dir(workDir))

	epub, err := openEpub(book.EpubPath)
	if err != nil {
		return err
	}
	filter := chapterFilter{
		MinTextChars: cfg.MinTextChars,
		SkipPatterns: cfg.SkipPatterns,
		KeepPatterns: cfg.KeepPatterns,
	}
	chapters := epub.selectedChapters(filter)
	if len(chapters) == 0 {
		return fmt.Errorf("no readable chapters found in %s", book.EpubPath)
	}
	log.Printf("'%s' by %s: %d chapters", epub.meta.Title, epub.meta.Creator, len(chapters))
	for i, ch := range chapters {
		log.Printf("  %d. %s (%d characters)", i+1, ch.Title, ch.charCount())
	}

	names, err := epub.writeDerivedEpub(filepath.Join(workDir, derivedEpubName), chapters)
	if err != nil {
		return fmt.Errorf("could not build the chapter only epub: %w", err)
	}
	log.Printf("Running audiblez on %s (voice %s) -> %s", derivedEpubName, cfg.Voice, names[0])

	status := &conversionStatus{}
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				pct, eta := status.snapshot()
				log.Printf("'%s': progress %d%%, remaining %s", book.BookName, pct, eta)
			}
		}
	}()
	audiblezErr := runAudiblez(workDir, cfg, status)
	close(done)
	pct, eta := status.snapshot()
	book.Progress = pct
	book.TimeRemaining = eta
	if audiblezErr != nil {
		// audiblez still writes the wav files of every chapter it managed to
		// synthesise; the m4b step in particular fails on ffmpeg builds without
		// libfdk_aac. Keep whatever audio exists.
		log.Printf("%v - converting the wav files that were produced", audiblezErr)
	}

	audio, err := collectChapterAudio(workDir, chapters, cfg.Bitrate)
	if err != nil {
		if audiblezErr != nil {
			return fmt.Errorf("%w (audiblez: %v)", err, audiblezErr)
		}
		return err
	}
	if len(audio) < len(chapters) {
		log.Printf("Only %d of %d chapters have audio", len(audio), len(chapters))
	}
	book.Chapters = len(audio)

	if cfg.BuildM4B {
		m4bName := strings.TrimSuffix(book.BookName, filepath.Ext(book.BookName)) + ".m4b"
		if err := buildM4B(workDir, audio, epub.meta, m4bName, cfg.Bitrate); err != nil {
			log.Printf("Could not create the single file audiobook: %v", err)
		}
	}

	// The epub itself is copied into the output folder: it marks the book as
	// converted on the next scan.
	if err := os.MkdirAll(bookFolder, 0o755); err != nil {
		return fmt.Errorf("could not create destination directory %s: %w", bookFolder, err)
	}
	if err := copyFile(book.EpubPath, filepath.Join(bookFolder, book.BookName)); err != nil {
		return err
	}
	exclude := map[string]bool{
		derivedEpubName:       true, // our intermediate epub
		"cover":               true, // extracted by audiblez for its own m4b
		"audiobookconcat.txt": true,
		"chapters.ffmeta":     true,
		"chapters.m4b":        true, // audiblez's own (unnamed) audiobook
		"chapters.tmp.mp4":    true,
	}
	if err := moveFiles(workDir, bookFolder, exclude, []string{".wav", ".txt"}); err != nil {
		return err
	}
	log.Printf("Audio written to %s", bookFolder)
	return nil
}

// scanFolder registers the epub files of a folder as books.
func scanFolder(books map[string]*Book, cfg config, folder string, isSource bool) {
	files, err := traverseAndFindEpub(folder, cfg.MaxDepth)
	if err != nil {
		log.Printf("Failed to traverse '%s': %s", folder, err)
		return
	}
	kind := "output"
	if isSource {
		kind = "input"
	}
	log.Printf("Found %d epub files in %s folder %s", len(files), kind, folder)
	for _, epub := range files {
		basename := filepath.Base(epub)
		book, ok := books[basename]
		if !ok {
			book = &Book{BookName: basename, EpubPath: epub, Status: Error}
			books[basename] = book
		}
		if isSource {
			book.SourceFolder = filepath.Dir(epub)
			book.EpubPath = epub
			book.Status = New
		} else {
			book.TargetFolder = filepath.Dir(epub)
		}
		// A book whose epub also sits in the output folder has been converted.
		if book.TargetFolder != "" {
			book.Status = Done
		} else if book.SourceFolder == "" {
			book.Status = Error
		}
	}
}

func printBooks(books map[string]*Book) {
	for name, book := range books {
		extra := ""
		if book.Status == InProcess || book.Status == Done {
			extra = fmt.Sprintf(" chapters: %d progress: %d%% remaining: %s", book.Chapters, book.Progress, book.TimeRemaining)
		}
		if book.Status == Error && book.LastError != "" {
			extra = " reason: " + book.LastError
		}
		log.Printf("Book '%s' status: %s%s", name, book.Status, extra)
	}
}

// defaultConfig is the configuration without any environment input.
func defaultConfig() config {
	patterns, err := compilePatterns(defaultSkipPatterns)
	if err != nil {
		log.Fatalf("Invalid default skip pattern: %v", err)
	}
	return config{
		MaxDepth:     defaultMaxDepth,
		Voice:        defaultVoice,
		Speed:        1.0,
		Bitrate:      defaultBitrate,
		MinTextChars: defaultMinText,
		BuildM4B:     true,
		MaxAttempts:  defaultMaxTry,
		PollInterval: time.Duration(defaultPollMins) * time.Minute,
		SkipPatterns: patterns,
	}
}

// loadConfig reads the environment and checks the tools we depend on.
func loadConfig() config {
	base := defaultConfig()
	viper.AutomaticEnv()
	viper.SetEnvPrefix(ENV_PREFIX)
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	viper.SetDefault("voice", base.Voice)
	viper.SetDefault("bitrate", base.Bitrate)
	viper.SetDefault("speed", base.Speed)
	viper.SetDefault("max_depth", base.MaxDepth)
	viper.SetDefault("min_text_chars", base.MinTextChars)
	viper.SetDefault("max_attempts", base.MaxAttempts)
	viper.SetDefault("m4b", base.BuildM4B)
	viper.SetDefault("poll_minutes", int(base.PollInterval.Minutes()))

	cfg := config{
		FolderIn:     viper.GetString("in"),
		FolderOut:    viper.GetString("out"),
		MaxDepth:     viper.GetInt("max_depth"),
		Voice:        viper.GetString("voice"),
		Speed:        viper.GetFloat64("speed"),
		Bitrate:      viper.GetString("bitrate"),
		MinTextChars: viper.GetInt("min_text_chars"),
		BuildM4B:     viper.GetBool("m4b"),
		MaxAttempts:  viper.GetInt("max_attempts"),
		PollInterval: time.Duration(viper.GetInt("poll_minutes")) * time.Minute,
	}
	// FOLDER_IN/FOLDER_OUT were the names used before the CONVERTER_ prefix was
	// applied consistently; keep accepting them.
	if cfg.FolderIn == "" {
		cfg.FolderIn = os.Getenv("FOLDER_IN")
	}
	if cfg.FolderOut == "" {
		cfg.FolderOut = os.Getenv("FOLDER_OUT")
	}
	if cfg.Speed < 0.5 || cfg.Speed > 2.0 {
		log.Printf("Ignoring speed %v, must be between 0.5 and 2.0", cfg.Speed)
		cfg.Speed = 1.0
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
	log.Printf("%s_MAX_DEPTH: %d, %s_VOICE: %s, %s_SPEED: %v, %s_BITRATE: %s",
		ENV_PREFIX, cfg.MaxDepth, ENV_PREFIX, cfg.Voice, ENV_PREFIX, cfg.Speed, ENV_PREFIX, cfg.Bitrate)
	log.Printf("%s_MIN_TEXT_CHARS: %d, %s_M4B: %v, %s_POLL_MINUTES: %v, %s_MAX_ATTEMPTS: %d",
		ENV_PREFIX, cfg.MinTextChars, ENV_PREFIX, cfg.BuildM4B, ENV_PREFIX, cfg.PollInterval, ENV_PREFIX, cfg.MaxAttempts)

	if cfg.FolderIn == "" || cfg.FolderOut == "" {
		log.Fatalf("Error: %s_IN and %s_OUT must both be set", ENV_PREFIX, ENV_PREFIX)
	}
	for _, tool := range []string{"audiblez", "ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			log.Fatalf("Error: could not find the '%s' executable: %v", tool, err)
		}
	}
	if err := os.MkdirAll(cfg.FolderOut, 0o755); err != nil {
		log.Fatalf("Error: could not create %s_OUT: %v", ENV_PREFIX, err)
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

// traverseAndFindEpub finds .epub files below path, descending at most maxDepth
// directory levels (maxDepth == -1 for no limit). Depth is counted relative to
// path, not to the file system root.
func traverseAndFindEpub(path string, maxDepth int) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	root := filepath.Clean(path)
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			// Nothing scanned yet: an absent folder is an empty one.
			return nil, nil
		}
		return nil, err
	}
	var epubFiles []string
	err := filepath.WalkDir(root, func(currentPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, currentPath)
		if err != nil {
			return err
		}
		depth := 0
		if rel != "." {
			depth = strings.Count(rel, string(filepath.Separator)) + 1
		}
		if d.IsDir() {
			if maxDepth != -1 && depth > maxDepth {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(strings.ToLower(d.Name()), ".epub") {
			epubFiles = append(epubFiles, currentPath)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to traverse directory: %w", err)
	}
	return epubFiles, nil
}
