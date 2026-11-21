package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// The speech model is not part of the image or the repository; it is fetched
// from Hugging Face the first time it is needed (about 1.1 GB for the 0.6 B
// model, 4 GB for the 1.7 B one) and kept on disk afterwards. The file list
// mirrors the engine repository's download_model.sh.
const (
	defaultTTSModelID = "Qwen/Qwen3-TTS-12Hz-0.6B-CustomVoice"
)

// modelBaseURL is overridable in tests; the converter itself never points
// anywhere else.
var modelBaseURL = "https://huggingface.co"

// modelRepos are the CustomVoice/Base/VoiceDesign repositories the engine can
// run; the key is what CONVERTER_TTS_MODEL_ID accepts, with or without the
// "Qwen/" namespace prefix.
var modelRepos = map[string]bool{
	"Qwen/Qwen3-TTS-12Hz-0.6B-CustomVoice": true,
	"Qwen/Qwen3-TTS-12Hz-1.7B-CustomVoice": true,
	"Qwen/Qwen3-TTS-12Hz-1.7B-VoiceDesign": true,
	"Qwen/Qwen3-TTS-12Hz-0.6B-Base":        true,
	"Qwen/Qwen3-TTS-12Hz-1.7B-Base":        true,
}

// modelFiles are the entries every model repository holds; the engine reads
// them straight from the directory (there is no archive step).
var modelFiles = []string{
	"config.json",
	"generation_config.json",
	"tokenizer_config.json",
	"preprocessor_config.json",
	"model.safetensors",
	"vocab.json",
	"merges.txt",
	"speech_tokenizer/config.json",
	"speech_tokenizer/configuration.json",
	"speech_tokenizer/model.safetensors",
	"speech_tokenizer/preprocessor_config.json",
}

// normalizeModelID accepts "0.6B-CustomVoice" as well as
// "Qwen/Qwen3-TTS-12Hz-0.6B-CustomVoice" and returns the full repository name.
func normalizeModelID(id string) (string, error) {
	id = strings.TrimSpace(id)
	if !strings.Contains(id, "/") {
		id = "Qwen/Qwen3-TTS-12Hz-" + id
	}
	if !modelRepos[id] {
		names := make([]string, 0, len(modelRepos))
		for k := range modelRepos {
			names = append(names, k)
		}
		slices.Sort(names)
		return "", fmt.Errorf("unknown model %q, expected one of: %s", id, strings.Join(names, ", "))
	}
	return id, nil
}

// modelCacheDir is where a model lands when CONVERTER_TTS_MODEL is not set:
// the user cache below a name derived from the repository, so several models
// can live side by side.
func modelCacheDir(modelID string) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	tail := modelID[strings.LastIndex(modelID, "/")+1:]
	return filepath.Join(cache, "qwen3-tts", strings.ToLower(tail)), nil
}

// ensureModel makes dir a usable model directory: a present config.json means
// done, everything else triggers a download of the model repository into it.
// Downloads are resumable (a leftover .part continues with a Range request),
// so an interrupted gigabyte is not lost.
func ensureModel(dir, modelID string) error {
	if fi, err := os.Stat(filepath.Join(dir, "config.json")); err == nil && fi.Mode().IsRegular() {
		return nil
	}
	repo, err := normalizeModelID(modelID)
	if err != nil {
		return err
	}
	log.Printf("No model in %s - downloading %s from %s (once, %s)",
		dir, repo, modelBaseURL, modelSizeHint(repo))
	if err := os.MkdirAll(filepath.Join(dir, "speech_tokenizer"), 0o755); err != nil {
		return err
	}
	for _, name := range modelFiles {
		if err := downloadModelFile(repo, name, filepath.Join(dir, filepath.FromSlash(name))); err != nil {
			return fmt.Errorf("could not download %s: %w", name, err)
		}
	}
	log.Printf("Model %s is ready in %s", repo, dir)
	return nil
}

// modelSizeHint only feeds the log line before the download starts.
func modelSizeHint(repo string) string {
	if strings.Contains(repo, "1.7B") {
		return "about 5 GB"
	}
	return "about 2.4 GB"
}

// downloadModelFile fetches one repository file into dest unless dest already
// exists; a half-written dest.part is resumed.
func downloadModelFile(repo, name, dest string) error {
	if fi, err := os.Stat(dest); err == nil && fi.Mode().IsRegular() {
		return nil
	}
	part := dest + ".part"
	var from int64
	if fi, err := os.Stat(part); err == nil {
		from = fi.Size()
	}
	url := fmt.Sprintf("%s/%s/resolve/main/%s", modelBaseURL, repo, name)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if from > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", from))
		log.Printf("Resuming %s at %d bytes", name, from)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		from = 0 // no resume support: start over
	case http.StatusPartialContent:
	case http.StatusRequestedRangeNotSatisfiable:
		// The .part is already the whole file; finish the rename that was lost.
		if from == 0 {
			return fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return os.Rename(part, dest)
	default:
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	flags := os.O_CREATE | os.O_WRONLY
	if from == 0 {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if from > 0 {
		if _, err := f.Seek(from, io.SeekStart); err != nil {
			return err
		}
	}
	if err := copyWithProgress(f, resp.Body, resp.ContentLength, name); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(part, dest)
}

// copyWithProgress copies the body and reports a line whenever the transfer
// crosses the next quartile - a gigabyte download must not look like a hang.
// Unknown length reports bytes once a minute instead.
func copyWithProgress(dst io.Writer, src io.Reader, length int64, name string) error {
	var done int64
	step := length / 4
	if step < 1 {
		step = 1
	}
	next := step
	elseEvery := time.After(time.Minute)
	buf := make([]byte, 64*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
			done += int64(n)
			if length > 0 {
				if done >= next {
					pct := 100
					if done < length {
						pct = int(100 * done / length)
					}
					log.Printf("Downloading %s: %d%% (%d of %d MB)",
						name, pct, done>>20, length>>20)
					for next <= done && next < length {
						next += step
					}
				}
			} else {
				select {
				case <-elseEvery:
					log.Printf("Downloading %s: %d MB", name, done>>20)
					elseEvery = time.After(time.Minute)
				default:
				}
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
