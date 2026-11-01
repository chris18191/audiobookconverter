package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// conversionStatus is updated from the audiblez output while it runs.
type conversionStatus struct {
	mu        sync.Mutex
	percent   int
	eta       string
	processed int
	total     int
	lastLog   time.Time
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

var (
	etaRE      = regexp.MustCompile(`Estimated time remaining[^:]*:\s*(.+?)\s*$`)
	progressRE = regexp.MustCompile(`Progress:\s*(\d+)\s*%`)
)

// scan folds one line of audiblez output into the status and reports whether the
// line was progress information. Everything else belongs in the log.
func (s *conversionStatus) scan(line string) bool {
	if m := etaRE.FindStringSubmatch(line); m != nil {
		percent, _ := s.snapshot()
		s.set(percent, m[1])
		return true
	}
	if m := progressRE.FindStringSubmatch(line); m != nil {
		percent, err := strconv.Atoi(m[1])
		if err == nil {
			_, eta := s.snapshot()
			s.set(percent, eta)
		}
		return true
	}
	return false
}

// runAudiblez converts the derived epub into one wav file per chapter. Progress
// lines are folded into status instead of flooding the log; everything else is
// logged verbatim.
func runAudiblez(workDir string, cfg config, status *conversionStatus) error {
	// audiblez names its wav files after the epub file name, so the derived name
	// must be passed as argument, not an absolute path.
	args := []string{"-v", cfg.Voice, "-s", formatSpeed(cfg.Speed), "-o", ".", derivedEpubName}
	cmd := exec.Command("audiblez", args...)
	cmd.Dir = workDir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("could not attach to audiblez stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("could not attach to audiblez stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("could not start audiblez: %w", err)
	}

	var wg sync.WaitGroup
	read := func(r io.Reader) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			if status.scan(line) {
				continue
			}
			log.Printf("audiblez: %s", line)
		}
		if err := sc.Err(); err != nil {
			log.Printf("audiblez output could not be read: %v", err)
		}
	}
	wg.Add(2)
	go read(stdout)
	go read(stderr)
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("audiblez failed: %w", err)
	}
	return nil
}

func formatSpeed(speed float64) string {
	return strconv.FormatFloat(speed, 'f', -1, 64)
}

// derivedWavRE matches the wav file names audiblez produces for our derived
// epub: chapters_chapter_<audiblez index>_<voice>_<sanitised path>.wav with the
// sanitised path ending in the canonical chapter name we wrote.
var derivedWavRE = regexp.MustCompile(`^chapters_chapter_(\d+)_(.+)_chapter_(\d+)\.[A-Za-z0-9]+\.wav$`)

// wavFallbackRE is used if audiblez ever changes the trailing part of its file
// names: the leading counter is then the only hint of the order, and audiblez
// counts its own chapters, so the pairing of file and title is a guess.
var wavFallbackRE = regexp.MustCompile(`^chapters_chapter_(\d+)_.*\.wav$`)

// collectChapterAudio maps the wav files audiblez wrote onto our chapters and
// converts them to mp3, named "<number> - <chapter title>.mp3".
func collectChapterAudio(workDir string, chapters []epubChapter, bitrate string) ([]chapterAudio, error) {
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return nil, fmt.Errorf("could not read work directory: %w", err)
	}
	byNumber := map[int]string{}
	byIndex := map[int]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".wav") {
			continue
		}
		if m := derivedWavRE.FindStringSubmatch(e.Name()); m != nil {
			idx, _ := strconv.Atoi(m[1])
			num, _ := strconv.Atoi(m[3])
			byNumber[num] = e.Name()
			byIndex[idx] = e.Name()
			continue
		}
		if m := wavFallbackRE.FindStringSubmatch(e.Name()); m != nil {
			idx, _ := strconv.Atoi(m[1])
			if _, ok := byIndex[idx]; !ok {
				byIndex[idx] = e.Name()
			}
		}
	}

	out := make([]chapterAudio, 0, len(chapters))
	seen := map[string]bool{}
	for i, ch := range chapters {
		name, ok := byNumber[i+1]
		if !ok {
			// Fall back to audiblez's own counter. It skips empty chapters, so
			// the mapping degrades gracefully instead of mislabelling files.
			name, ok = byIndex[i+1]
			if ok {
				log.Printf("chapter %d (%s) matched by the audiblez counter, not by name: %s",
					i+1, ch.Title, name)
			}
		}
		if !ok || seen[name] {
			log.Printf("no audio was produced for chapter %d (%s)", i+1, ch.Title)
			continue
		}
		seen[name] = true
		mp3Name := fmt.Sprintf("%0*d - %s.mp3", numberWidth(len(chapters)), i+1, sanitizeFileName(ch.Title))
		mp3Path := filepath.Join(workDir, mp3Name)
		if err := convertToMP3(filepath.Join(workDir, name), mp3Path, bitrate); err != nil {
			return nil, err
		}
		out = append(out, chapterAudio{
			Title:    ch.Title,
			MP3File:  mp3Name,
			Duration: probeDuration(mp3Path),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("audiblez produced no chapter audio in %s", workDir)
	}
	return out, nil
}

type chapterAudio struct {
	Title    string
	MP3File  string
	Duration float64
}

func convertToMP3(wavPath, mp3Path, bitrate string) error {
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-i", filepath.Base(wavPath), "-ac", "2", "-b:a", bitrate, filepath.Base(mp3Path))
	cmd.Dir = filepath.Dir(wavPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("could not convert %s to mp3: %w: %s", filepath.Base(wavPath), err, strings.TrimSpace(string(out)))
	}
	return nil
}

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

// buildM4B concatenates the chapter mp3s into a single chaptered .m4b. audiblez
// builds its own m4b with the libfdk_aac encoder, which the ffmpeg build of the
// Debian slim images does not provide, so relying on it would silently leave us
// without an audiobook file - and with "Chapter 0/1/2" as chapter titles.
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
		fmt.Fprintf(&list, "file '%s'\n", strings.ReplaceAll(c.MP3File, "'", `'\''`))
		dur := int64(c.Duration * 1000)
		if dur <= 0 {
			return fmt.Errorf("could not determine the duration of %s", c.MP3File)
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

// numberWidth is the zero padding needed to sort chapter numbers by name.
func numberWidth(total int) int {
	if total < 10 {
		return 1
	}
	return len(strconv.Itoa(total))
}

// moveFiles copies the finished files into the destination directory. The mp3
// files of an earlier attempt that this run did not produce again are removed:
// a retried book must not keep the audio of a previous attempt beside its own.
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
		if file.IsDir() || copied[name] || !strings.EqualFold(filepath.Ext(name), ".mp3") {
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
