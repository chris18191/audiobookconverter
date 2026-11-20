package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// conversionStatus is the progress of the book that is converting right now.
type conversionStatus struct {
	mu      sync.Mutex
	percent int
	eta     string
}

func (s *conversionStatus) set(percent int, eta string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.percent = percent
	s.eta = eta
}

func (s *conversionStatus) snapshot() (int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.percent, s.eta
}

// chapterAudio is one chapter of the book, in reading order, as the engine
// wrote it.
type chapterAudio struct {
	Title    string
	File     string // the wav file, relative to the work directory
	Duration float64
}

// probeDuration returns the playable length of an audio file in seconds.
func probeDuration(file string) float64 {
	cmd := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", file)
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0
	}
	return d
}

// buildM4B concatenates the chapter wavs into a single chaptered .m4b. The
// encoder is the ffmpeg-native aac: the Debian slim images do not provide
// libfdk_aac, so relying on it would silently leave us without an audiobook
// file - and with "Chapter 0/1/2" as chapter titles.
func buildM4B(workDir string, chapters []chapterAudio, meta bookMetadata, destName, bitrate string) error {
	listPath := filepath.Join(workDir, "audiobookconcat.txt")
	metaPath := filepath.Join(workDir, "chapters.ffmeta")

	var list strings.Builder
	var md strings.Builder
	md.WriteString(";FFMETADATA1\n")
	if meta.Title != "" {
		fmt.Fprintf(&md, "title=%s\n", ffmetaEscape(meta.Title))
	}
	if meta.Creator != "" {
		fmt.Fprintf(&md, "artist=%s\n", ffmetaEscape(meta.Creator))
	}
	md.WriteString("\n")
	start := int64(0)
	for _, c := range chapters {
		fmt.Fprintf(&list, "file '%s'\n", strings.ReplaceAll(c.File, "'", `'\''`))
		dur := int64(c.Duration * 1000)
		if dur <= 0 {
			return fmt.Errorf("could not determine the duration of %s", c.File)
		}
		// The title only: epub chapter titles carry their own numbering
		// ("1. Origins"), a second prefix would read "1. 1. Origins".
		fmt.Fprintf(&md, "[CHAPTER]\nTIMEBASE=1/1000\nSTART=%d\nEND=%d\ntitle=%s\n\n",
			start, start+dur, ffmetaEscape(c.Title))
		start += dur
	}
	if err := os.WriteFile(listPath, []byte(list.String()), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(metaPath, []byte(md.String()), 0o644); err != nil {
		return err
	}
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "concat", "-safe", "0", "-i", filepath.Base(listPath),
		"-i", filepath.Base(metaPath),
		"-map", "0:a", "-map_metadata", "1", "-map_chapters", "1",
		"-c:a", "aac", "-b:a", bitrate, "-movflags", "+faststart",
		destName)
	cmd.Dir = workDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("could not create %s: %w: %s", destName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ffmetaEscape escapes a value for an ffmpeg metadata file.
func ffmetaEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `=`, `\=`, "\n", `\n`, ";", `\;`, "#", `\#`)
	return r.Replace(s)
}

// maxTitleLen caps chapter titles in file names: file systems allow more, but a
// whole paragraph as a name is useless.
const maxTitleLen = 100

// sanitizeFileName turns a chapter title into a safe, sorted file name.
func sanitizeFileName(title string) string {
	var b strings.Builder
	prevSpace := false
	for _, r := range title {
		switch {
		case r < 0x20 || r == 0x7f:
			r = ' '
		case r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' ||
			r == '"' || r == '<' || r == '>' || r == '|' || r == 0:
			r = '-'
		}
		if r == ' ' || r == '\t' {
			if !prevSpace && b.Len() > 0 {
				b.WriteByte(' ')
			}
			prevSpace = true
			continue
		}
		prevSpace = false
		b.WriteRune(r)
	}
	name := strings.TrimSpace(b.String())
	name = strings.TrimLeft(name, ".")
	name = strings.TrimRight(name, ". ")
	if utf8Len(name) > maxTitleLen {
		name = strings.TrimSpace(string([]rune(name)[:maxTitleLen]))
	}
	if name == "" {
		name = "chapter"
	}
	return name
}

func utf8Len(s string) int { return len([]rune(s)) }

// moveFiles copies the finished files into the destination directory. Audio of an
// earlier attempt that this run did not produce again is removed: a retried book
// must not keep the audio of a previous attempt beside its own - which includes
// the chapter mp3 files an older version of the converter wrote there.
func moveFiles(srcDir string, destDir string, excludeNames map[string]bool, excludeExtensions []string) error {
	files, err := os.ReadDir(srcDir)
	if err != nil {
		return fmt.Errorf("could not read source directory %s: %w", srcDir, err)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("could not create destination directory %s: %w", destDir, err)
	}
	copied := map[string]bool{}
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		name := file.Name()
		if excludeNames[name] {
			continue
		}
		if contains(excludeExtensions, strings.ToLower(filepath.Ext(name))) {
			continue
		}
		src := filepath.Join(srcDir, name)
		dest := filepath.Join(destDir, name)
		if err := copyFile(src, dest); err != nil {
			return err
		}
		copied[name] = true
	}
	leftovers, err := os.ReadDir(destDir)
	if err != nil {
		return fmt.Errorf("could not read destination directory %s: %w", destDir, err)
	}
	for _, file := range leftovers {
		name := file.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if file.IsDir() || copied[name] || (ext != ".mp3" && ext != ".m4b") {
			continue
		}
		if err := os.Remove(filepath.Join(destDir, name)); err != nil {
			return fmt.Errorf("could not remove the audio of an earlier attempt %s: %w", name, err)
		}
		log.Printf("Removed '%s': this attempt did not produce it", name)
	}
	return nil
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("could not open %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("could not create %s: %w", dest, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("could not write %s: %w", dest, err)
	}
	return out.Close()
}

func contains(list []string, v string) bool {
	for _, e := range list {
		if e == v {
			return true
		}
	}
	return false
}
