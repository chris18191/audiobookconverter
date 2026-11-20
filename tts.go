package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

// ttsEngine is the local Qwen3-TTS engine (gabriele-mastrapasqua/qwen3-tts)
// behind its HTTP server. The server loads the weights once and keeps them,
// the tokenizer and the embedding caches warm across requests, so the model
// load is paid once per converter run instead of once per segment. By default
// the converter starts the server as a child process and stops it on exit;
// CONVERTER_TTS_URL points the converter at a server someone else runs.
type ttsEngine struct {
	cfg    config
	client *http.Client

	mu      sync.Mutex
	baseURL string    // engine server in use, empty until it answers health
	cmd     *exec.Cmd // our child process, nil for an external server
	cmdErr  chan error
}

// engineRequestTimeout bounds one synthesis request on the client side. The
// server caps its own generation at engineMaxRequestSeconds; the client gives
// up a little later so the server's error surfaces in the log.
const (
	engineRequestTimeout    = 6 * time.Minute
	engineMaxRequestSeconds = 300
)

func newTTSEngine(cfg config) *ttsEngine {
	return &ttsEngine{cfg: cfg, client: &http.Client{Timeout: engineRequestTimeout}}
}

// external reports whether the engine is a server the converter does not own.
func (e *ttsEngine) external() bool { return e.cfg.TTSURL != "" }

// ensure makes the engine reachable: the child is started when missing or
// dead, an external server is health-checked. Called before every book, so a
// server that died between books is noticed here and not at the first segment.
func (e *ttsEngine) ensure() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.external() {
		url := strings.TrimSuffix(e.cfg.TTSURL, "/")
		e.baseURL = url
		if err := e.health(); err != nil {
			e.baseURL = ""
			return fmt.Errorf("engine server at %s is not healthy: %w", url, err)
		}
		return nil
	}
	if processAlive(e.cmd) {
		return nil
	}
	return e.startLocked()
}

// startLocked spawns the engine server and waits for its health check. The
// server's own log lines go into the converter log with an "engine:" prefix;
// if the child dies during startup, the tail of that output names the reason.
func (e *ttsEngine) startLocked() error {
	e.stopLocked()

	port, err := freeTCPPort()
	if err != nil {
		return err
	}
	args := []string{
		"-d", e.cfg.TTSModelDir,
		"--serve", strconv.Itoa(port),
		// A segment is at most TTS_MAX_CHARS (≤ 1792, the server's text
		// limit) and generation must not be cut off mid-sentence: bound it
		// generously instead of at the server's 60 s default.
		"--max-request-seconds", strconv.Itoa(engineMaxRequestSeconds),
	}
	switch e.cfg.TTSQuantize {
	case "int8":
		args = append(args, "--int8")
	case "mixed-cpu":
		args = append(args, "--quant-mixed-cpu")
	}
	if e.cfg.TTSThreads > 0 {
		args = append(args, "-j", strconv.Itoa(e.cfg.TTSThreads))
	}

	cmd := exec.Command(e.cfg.TTSBin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("could not attach to the engine's stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("could not attach to the engine's stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("could not start the engine: %w", err)
	}
	go func() {
		for range logEngineOutput(stdout) {
		}
	}()
	tailCh := logEngineOutput(stderr)
	e.cmd = cmd
	e.cmdErr = make(chan error, 1)
	go func() { e.cmdErr <- cmd.Wait() }()
	e.baseURL = fmt.Sprintf("http://127.0.0.1:%d", port)

	deadline := time.Now().Add(time.Duration(e.cfg.TTSStartup) * time.Second)
	for {
		if err := e.health(); err == nil {
			log.Printf("Qwen3-TTS engine listening on %s", e.baseURL)
			return nil
		}
		select {
		case err := <-e.cmdErr:
			return fmt.Errorf("the engine exited during startup (%s): %s", err, <-tailCh)
		case <-time.After(500 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			e.stopLocked()
			return fmt.Errorf("the engine did not become ready within %ds - the model load may need more %s_TTS_STARTUP_SECONDS",
				e.cfg.TTSStartup, ENV_PREFIX)
		}
	}
}

// health asks the engine's health endpoint; any 2xx means it can take work.
func (e *ttsEngine) health() error {
	resp, err := e.client.Get(e.baseURL + "/v1/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("health check answered %s", resp.Status)
	}
	return nil
}

// logEngineOutput folds the engine server's output into the converter log and
// yields the last output line once the stream ends (startup failure messages
// name the engine's own complaint).
func logEngineOutput(r io.Reader) chan string {
	out := make(chan string, 1)
	go func() {
		var last string
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" {
				last = line
				log.Printf("engine: %s", line)
			}
		}
		out <- tail(last, 400)
	}()
	return out
}

// freeTCPPort reserves an unused localhost port for the engine server.
func freeTCPPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("could not find a free port for the engine: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// stop terminates the child process; an external server is left alone.
func (e *ttsEngine) stop() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopLocked()
}

func (e *ttsEngine) stopLocked() {
	if processAlive(e.cmd) {
		_ = e.cmd.Process.Kill()
		<-e.cmdErr
	}
	e.cmd = nil
	e.baseURL = ""
}

// processAlive reports whether a spawned command is still running. Signal 0
// performs no operation; it only fails when the process no longer exists
// (after our Wait goroutine has reaped it).
func processAlive(cmd *exec.Cmd) bool {
	return cmd != nil && cmd.Process != nil && cmd.Process.Signal(syscall.Signal(0)) == nil
}

// synthesize turns the selected chapters into one wav per chapter. The
// chapter text is split into segments that one engine request can handle,
// each segment is synthesized to a wav, and the segments are concatenated
// with a short silence between them. A segment failure fails the book: the
// work directory is wiped and the whole book is retried on the next scan.
func (e *ttsEngine) synthesize(workDir string, epub *epubBook, chapters []epubChapter, status *conversionStatus) ([]chapterAudio, error) {
	cfg := e.cfg
	lang, reason := bookLanguage(cfg.TTSLanguage, epub.meta.Language, languageSample(chapters, 4096))
	quantize := "full precision"
	if cfg.TTSQuantize != "" {
		quantize = cfg.TTSQuantize
	}
	if e.external() {
		log.Printf("Speech language %s (%s), narrator %q, engine server %s", lang, reason, cfg.TTSSpeaker, cfg.TTSURL)
	} else {
		log.Printf("Speech language %s (%s), narrator %q, %s", lang, reason, cfg.TTSSpeaker, quantize)
	}
	if err := e.ensure(); err != nil {
		return nil, err
	}

	plans := make([][]string, len(chapters))
	total := 0
	for i, ch := range chapters {
		plans[i] = segmentsForText(ch.Text, cfg.TTSMaxChars, lang)
		total += len(plans[i])
	}
	if total == 0 {
		return nil, fmt.Errorf("no speakable text found in the %d selected chapters", len(chapters))
	}

	var audio []chapterAudio
	started := time.Now()
	done := 0
	for ci, segs := range plans {
		files := make([]string, 0, len(segs))
		for si, seg := range segs {
			name := fmt.Sprintf("seg_%03d_%04d.wav", ci+1, si+1)
			if err := e.synthesizeSegment(seg, filepath.Join(workDir, name), lang); err != nil {
				return nil, fmt.Errorf("chapter %d (%s) segment %d/%d: %w",
					ci+1, chapters[ci].Title, si+1, len(segs), err)
			}
			files = append(files, name)
			done++
			avg := time.Since(started) / time.Duration(done)
			status.set(done*100/total, (avg * time.Duration(total-done)).Round(time.Second).String())
		}
		out := fmt.Sprintf("chapter_%03d.wav", ci+1)
		if err := assembleSegments(workDir, files, out, cfg.TTSSilenceMS); err != nil {
			return nil, fmt.Errorf("chapter %d (%s): %w", ci+1, chapters[ci].Title, err)
		}
		dur := probeDuration(filepath.Join(workDir, out))
		audio = append(audio, chapterAudio{Title: chapters[ci].Title, File: out, Duration: dur})
		log.Printf("Chapter %d/%d \"%s\": %d segments, %.1f seconds of audio",
			ci+1, len(chapters), chapters[ci].Title, len(segs), dur)
	}
	log.Printf("Qwen3-TTS synthesized %d segments in %s", total, time.Since(started).Round(time.Second))
	return audio, nil
}

// ttsRequest is the /v1/tts body. Fields the engine resets per request are
// only sent when they differ from the engine defaults.
type ttsRequest struct {
	Text     string   `json:"text"`
	Speaker  string   `json:"speaker"`
	Language string   `json:"language"`
	Rate     *float64 `json:"rate,omitempty"`
	Seed     *int64   `json:"seed,omitempty"`
}

// synthesizeSegment asks the engine for one wav file of speech. A connection
// failure with a child server means it died: it is restarted once and the
// segment retried before the book fails.
func (e *ttsEngine) synthesizeSegment(text, outPath, lang string) error {
	body, err := json.Marshal(e.request(text, lang))
	if err != nil {
		return err
	}
	wav, retry, err := e.postTTS(body)
	if err != nil {
		if !retry {
			return err
		}
		log.Printf("engine connection failed (%v) - restarting the server and retrying the segment", err)
		e.mu.Lock()
		startErr := e.startLocked()
		e.mu.Unlock()
		if startErr != nil {
			return fmt.Errorf("%v, and the restart failed too: %w", err, startErr)
		}
		if wav, _, err = e.postTTS(body); err != nil {
			return err
		}
	}
	if len(wav) < 100 {
		return fmt.Errorf("the engine reported success but sent %d bytes of audio", len(wav))
	}
	if err := os.WriteFile(outPath, wav, 0o644); err != nil {
		return fmt.Errorf("could not write %s: %w", filepath.Base(outPath), err)
	}
	return nil
}

func (e *ttsEngine) request(text, lang string) ttsRequest {
	req := ttsRequest{Text: text, Speaker: e.cfg.TTSSpeaker, Language: lang}
	if e.cfg.Speed != 1.0 {
		r := e.cfg.Speed
		req.Rate = &r
	}
	if e.cfg.TTSSeed >= 0 {
		s := e.cfg.TTSSeed
		req.Seed = &s
	}
	return req
}

// postTTS runs one request against /v1/tts. The bool says whether the caller
// may retry after a restart: only connection-level failures with a child
// server qualify; a 4xx/5xx is an answer from a live engine and stays an error.
func (e *ttsEngine) postTTS(body []byte) ([]byte, bool, error) {
	resp, err := e.client.Post(e.baseURL+"/v1/tts", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, !e.external(), fmt.Errorf("engine request failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, !e.external(), fmt.Errorf("could not read the engine's answer: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, false, fmt.Errorf("engine answered %s: %s", resp.Status, tail(string(data), 400))
	}
	return data, false, nil
}

// tail returns the last n characters of s, for error messages.
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// segmentsForText splits a chapter's plain text into pieces the engine should
// receive in one call: at most maxChars characters, sentence boundaries only,
// quoted speech kept whole, paragraphs kept from being glued into one long
// breath. Text that contains no letters or digits ("* * *") is dropped.
func segmentsForText(text string, maxChars int, lang string) []string {
	var segments []string
	var cur strings.Builder
	quoteOpen := 0

	flush := func() {
		if seg := strings.Join(strings.Fields(cur.String()), " "); seg != "" && speakable(seg) {
			segments = append(segments, seg)
		}
		cur.Reset()
		quoteOpen = 0
	}

	for _, para := range strings.Split(text, "\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		for _, sentence := range splitSentences(para, lang) {
			glue := " "
			if cur.Len() == 0 {
				glue = ""
			}
			if cur.Len() > 0 && utf8Len(cur.String())+utf8Len(sentence)+1 > maxChars {
				// Do not end a segment in the middle of quoted speech: pull in
				// up to two more sentences even over budget to close the quote.
				if quoteOpen > 0 {
					segments = append(segments, cur.String()+glue+sentence)
					cur.Reset()
					quoteOpen = 0
					continue
				}
				flush()
			}
			for _, r := range sentence {
				switch r {
				case '„', '“', '«':
					quoteOpen++
				case '”', '»', '"':
					quoteOpen--
				}
			}
			if utf8Len(sentence) > maxChars {
				if cur.Len() > 0 {
					flush()
				}
				for _, piece := range splitOversized(sentence, maxChars) {
					if speakable(piece) {
						segments = append(segments, piece)
					}
				}
				continue
			}
			cur.WriteString(glue)
			cur.WriteString(sentence)
		}
		// Paragraphs never share a segment: the model treats one call as one
		// breath, and scene breaks deserve their own pause.
		flush()
	}
	return segments
}

// speakable reports whether the text holds at least one letter or digit;
// ornamental separators like "* * *" make the engine fail or hallucinate.
func speakable(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// abbreviationRE matches the abbreviations whose final dot must not end a
// sentence in English or German prose, including the two-part ones ("z. B.",
// "u. a.", "d. h.") and common titles. The matched dot is masked with \x01 and
// restored after sentence splitting.
var abbreviationRE = regexp.MustCompile(`(?i)(?P<short>\b(?:[a-z])\.\s[a-z])\.|\b(?:usw|bzw|etc|vgl|resp|evtl|tlw|sog|inkl|exkl|ca|ff|no|nr|dr|prof|mr|mrs|ms|hr|fr|cf|vs|st|al)\.`)

// sentenceEndRE finds terminal punctuation runs.
var sentenceEndRE = regexp.MustCompile(`[.!?…]+`)

// splitSentences splits one paragraph into sentences. A boundary needs
// punctuation plus whitespace plus the start of a new statement (uppercase,
// opening quote, digit) - decimals ("3.14") and abbreviation dots (masked, see
// abbreviationRE) stay inside their sentence, and a dot behind a single-letter
// initial ("J. K. Rowling") never cuts either.
func splitSentences(para string, lang string) []string {
	if para == "" {
		return nil
	}
	// Single-letter initials ("J. K. Rowling") are protected in boundaryAfter;
	// a sentence that truly ends on an initial is joined to its neighbour, the
	// safe direction of error.
	runes := []rune(maskAbbreviations(para))
	// A terminal dot inside an open quotation never cuts - that is how
	// „Geh.“, sagte er. and "Go.", she said. stay one sentence. „ opens and
	// ” closes in both languages; " closes German „…“ but opens English
	// “…”; straight " toggles.
	depth := make([]int, len(runes)+1)
	straight := false
	d := 0
	for k, r := range runes {
		switch r {
		case '„', '«':
			d++
		case '”', '»':
			d--
		case '“':
			if lang == langGerman {
				d--
			} else {
				d++
			}
		case '"':
			if straight {
				d--
				straight = false
			} else {
				d++
				straight = true
			}
		}
		if d < 0 {
			d = 0 // stray closing quote: never poison the rest of the paragraph
		}
		depth[k+1] = d
	}
	var sentences []string
	start := 0
	for i := 0; i < len(runes); i++ {
		if m := sentenceEndRE.FindStringIndex(string(runes[i:])); m == nil || m[0] != 0 {
			continue
		}
		end := i
		for end < len(runes) && strings.ContainsRune(".!?…", runes[end]) {
			end++
		}
		j := end
		for j < len(runes) && (runes[j] == ' ' || runes[j] == '\t') {
			j++
		}
		if depth[end] > 0 || !boundaryAfter(runes, i, j) {
			i = end - 1
			continue
		}
		sentences = append(sentences, unmask(string(runes[start:end])))
		start = j
		i = end - 1
	}
	if rest := strings.TrimSpace(unmask(string(runes[start:]))); rest != "" {
		sentences = append(sentences, rest)
	}
	return sentences
}

// boundaryAfter decides whether the punctuation run ending before index j
// closes a sentence.
func boundaryAfter(runes []rune, punctStart, j int) bool {
	if j >= len(runes) {
		return true // end of the paragraph
	}
	next := runes[j]
	// A straight quote never starts a sentence here: in German „…" it closes
	// the speech just ended; in English "…" the ambiguity is resolved the
	// safe way round, by joining two short sentences.
	if !(unicode.IsUpper(next) || next == '„' || next == '“' ||
		next == '«' || next == '\'' || unicode.IsDigit(next) || next == '[' || next == '(') {
		return false
	}
	// digit . digit is a decimal point.
	if punctStart > 0 && punctStart < len(runes) &&
		unicode.IsDigit(runes[punctStart-1]) && unicode.IsDigit(next) {
		return false
	}
	// single letter before the dot: an initial ("J. K. Rowling", "Er hieß J.")
	// rather than a sentence end; joining the rare sentence that truly ends on
	// an initial is the safe error.
	if punctStart >= 2 && unicode.IsLetter(runes[punctStart-1]) &&
		(runes[punctStart-2] == ' ' || punctStart == 2) {
		return false
	}
	return true
}

func maskAbbreviations(s string) string {
	return abbreviationRE.ReplaceAllStringFunc(s, func(m string) string {
		return m[:len(m)-1] + "\x01"
	})
}

func unmask(s string) string { return strings.ReplaceAll(s, "\x01", ".") }

// splitOversized cuts a sentence longer than maxChars at a comma-ish break near
// the budget, else at a space; never mid-word unless the word alone busts it.
// Cuts that would leave a quotation open are avoided; a balanced position wins
// even when it is far from the budget, because the engine recovers gracefully
// from a short piece but stammers through a torn dialogue.
func splitOversized(sentence string, maxChars int) []string {
	var pieces []string
	rest := []rune(sentence)
	for utf8Len(string(rest)) > maxChars {
		cut := -1
		for i := maxChars; i > 1; i-- {
			switch rest[i-1] {
			case ',', ';', ':', ' ', '„', '“':
			default:
				continue
			}
			if cut < 0 {
				cut = i // best position found so far, kept as fallback
			}
			if quoteBalance(rest[:i]) == 0 {
				cut = i
				break
			}
		}
		if cut < 0 {
			cut = maxChars
		}
		for cut > 1 && rest[cut-1] == ' ' {
			cut--
		}
		pieces = append(pieces, strings.TrimSpace(string(rest[:cut])))
		rest = rest[cut:]
	}
	if last := strings.TrimSpace(string(rest)); last != "" {
		pieces = append(pieces, last)
	}
	return pieces
}

// quoteBalance counts „…“ and «…» pairs: negative means a quotation is open.
func quoteBalance(runes []rune) int {
	open := 0
	for _, r := range runes {
		switch r {
		case '„', '«':
			open++
		case '”', '»':
			open--
		}
	}
	return open
}

// assembleSegments concatenates the segment wavs of one chapter into out (a
// file in workDir), separated by silenceMS of quiet. The engine writes
// pcm_s16le 24 kHz mono; the silence is generated to match.
func assembleSegments(workDir string, segs []string, out string, silenceMS int) error {
	if len(segs) == 0 {
		return fmt.Errorf("no segments to assemble")
	}
	if len(segs) == 1 {
		return os.Rename(filepath.Join(workDir, segs[0]), filepath.Join(workDir, out))
	}
	var list strings.Builder
	silence := ""
	if silenceMS > 0 {
		var err error
		if silence, err = ensureSilenceFile(workDir, filepath.Join(workDir, segs[0]), silenceMS); err != nil {
			return err
		}
	}
	for i, s := range segs {
		if i > 0 && silence != "" {
			fmt.Fprintf(&list, "file '%s'\n", silence)
		}
		fmt.Fprintf(&list, "file '%s'\n", s)
	}
	listName := "concat_" + out + ".txt"
	if err := os.WriteFile(filepath.Join(workDir, listName), []byte(list.String()), 0o644); err != nil {
		return err
	}
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "concat", "-safe", "0", "-i", listName, "-c:a", "pcm_s16le", out)
	cmd.Dir = workDir
	if outp, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("could not concatenate the segments: %w: %s", err, tail(string(outp), 300))
	}
	return nil
}

// ensureSilenceFile creates the shared silence wav once per work directory,
// matching the engine's sample rate and channel count (probed from the first
// segment). A different engine output format is a hard error: silently skipping
// the pauses would change the book's pacing.
func ensureSilenceFile(workDir, refWav string, ms int) (string, error) {
	name := fmt.Sprintf("silence_%dms.wav", ms)
	if _, err := os.Stat(filepath.Join(workDir, name)); err == nil {
		return name, nil
	}
	cmd := exec.Command("ffprobe", "-v", "error", "-select_streams", "a:0",
		"-show_entries", "stream=codec_name,sample_rate,channels", "-of", "csv=p=0", refWav)
	outp, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("could not probe %s: %w", filepath.Base(refWav), err)
	}
	fields := strings.Split(strings.TrimSpace(string(outp)), ",")
	if len(fields) != 3 || fields[0] != "pcm_s16le" {
		return "", fmt.Errorf("engine wrote %s in %q, expected pcm_s16le", filepath.Base(refWav), strings.TrimSpace(string(outp)))
	}
	rate, channels := fields[1], fields[2]
	layout := "mono"
	if channels == "2" {
		layout = "stereo"
	}
	gen := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("anullsrc=channel_layout=%s:sample_rate=%s", layout, rate),
		"-t", fmt.Sprintf("%.3f", float64(ms)/1000), "-c:a", "pcm_s16le", name)
	gen.Dir = workDir
	if outp, err := gen.CombinedOutput(); err != nil {
		return "", fmt.Errorf("could not create %s: %w: %s", name, err, tail(string(outp), 300))
	}
	return name, nil
}
