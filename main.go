package main

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
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

func main() {
	cfg := loadConfig()
	books := make(map[string]*Book)

	// The engine server outlives a single book (its warm weights are the
	// whole point) but must not outlive the converter: on Ctrl+C or systemd
	// stop, kill the child first so it cannot linger as an orphan with the
	// model loaded.
	eng := newTTSEngine(cfg)
	defer eng.stop()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigs
		log.Printf("Received %s, stopping...", sig)
		eng.stop()
		os.Exit(0)
	}()

	for {
		// Books in the import folder are candidates; a book is considered
		// finished once its epub has been copied next to the audio in the output
		// folder (that copy is what marks it as done).
		scanFolder(books, cfg, cfg.FolderIn, true)
		scanFolder(books, cfg, cfg.FolderOut, false)
		printBooks(books)
		processBooks(books, cfg, eng)

		log.Printf("Waiting %s for new files...", cfg.PollInterval)
		time.Sleep(cfg.PollInterval)
	}
}

// processBooks converts every book that has not been converted yet, one after
// another: synthesis is CPU bound and one book at a time keeps the log
// readable.
func processBooks(books map[string]*Book, cfg config, eng *ttsEngine) {
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
		if err := convertBook(book, cfg, eng); err != nil {
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

// convertBook selects the chapters, synthesizes them with the engine and
// writes title named audio to the output folder.
func convertBook(book *Book, cfg config, eng *ttsEngine) error {
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
	var audio []chapterAudio
	audio, err = eng.synthesize(workDir, epub, chapters, status)
	close(done)
	pct, eta := status.snapshot()
	book.Progress = pct
	book.TimeRemaining = eta
	if err != nil {
		return err
	}
	book.Chapters = len(audio)

	// The .m4b is the only audio file the converter delivers, so its failure is
	// the book's failure: without it the output folder would hold nothing but the
	// epub marker.
	m4bName := strings.TrimSuffix(book.BookName, filepath.Ext(book.BookName)) + ".m4b"
	if err := buildM4B(workDir, audio, epub.meta, m4bName, cfg.Bitrate); err != nil {
		return err
	}

	if err := os.MkdirAll(bookFolder, 0o755); err != nil {
		return fmt.Errorf("could not create destination directory %s: %w", bookFolder, err)
	}
	exclude := map[string]bool{
		"chapters.ffmeta": true, // buildM4B's own metadata file
	}
	if err := moveFiles(workDir, bookFolder, exclude, []string{".wav", ".txt"}); err != nil {
		return err
	}
	// The source epub is copied last: its presence in the output folder is what
	// marks the book as converted, and a failed copy of the audio must leave the
	// book retryable instead of finished without audio.
	if err := copyFile(book.EpubPath, filepath.Join(bookFolder, book.BookName)); err != nil {
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
			if other := book.EpubPath; other != "" && other != epub {
				// Two different epubs cannot share one output folder, so the
				// second one is reported instead of silently replacing the first.
				log.Printf("'%s' was already found at '%s': ignoring '%s'", basename, other, epub)
				continue
			}
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
