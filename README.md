# Audiobook Converter

Watches an input folder, converts the EPUBs it finds into audiobooks, and
writes the result to an output folder. The speech comes from a local
[Qwen3-TTS](https://github.com/QwenLM/Qwen3-TTS) engine
([pure C inference](https://github.com/gabriele-mastrapasqua/qwen3-tts)):
fully offline, good German and English voices, and the building blocks for
expressive narration (per-segment speech rate, emotion and instructions on
the 1.7 B models, voice cloning profiles).

TTS tools turn every readable document of an epub into speech. Left alone that
means a book full of cover pages, tables of contents, copyright notices,
glossaries and bibliographies - and audio files with internal zip paths for
names. This wrapper therefore selects the chapters itself, extracts their
text and names the audio after them.

## What it does

For each `<Book>.epub` in the input folder:

1. **Read the epub** - the OPF spine gives the reading order, the table of
   contents (EPUB3 `nav` or EPUB2 `toc.ncx`) gives the chapter titles, the
   first `<h1>`-`<h4>` of a document is the fallback title.
2. **Keep only readable chapters.** Dropped: tables of contents (including the
   generated `Contents` pages calibre emits), documents labelled with a
   non-content `epub:type` (`page-list`, `bibliography`, `index`, `colophon`,
   `cover`, ...), documents whose title matches a back matter pattern
   (bibliography, glossary, acknowledgments, about the author, ...), fragments
   the table of contents does not mention (the second half of a document calibre
   split in pieces), and documents too short to hold a chapter. For a book
   without any table of contents, length is the only clue, and a document with a
   chapter's worth of text is kept. The log lists every dropped document with
   the reason.
3. **Synthesize the chapters** in reading order with the engine: each chapter's
   text is split into segments (paragraphs are never merged, sentences are
   never cut), every segment is one request to a resident engine server that
   loaded the model once, and the configured silence fills the gaps between
   segments.
4. **Write one `<Book>.m4b`** holding the whole book, with the epub chapter
   titles as chapter markers at the right offsets. No other audio file is
   written: intermediate audio stays in the work directory.
5. **Copy the source epub** into the book's folder in the output directory. That
   copy is the marker of a finished book: the scanner never picks it up again as
   new work.

```
<out>/The Secret Name/
├── The Secret Name.m4b
└── The Secret Name.epub   <- finished marker
```

## Configuration

All settings are environment variables; the `CONVERTER_` prefix is optional for
the input and output folders (`FOLDER_IN`, `FOLDER_OUT` still work).

| Variable | Default | Meaning |
| --- | --- | --- |
| `CONVERTER_IN` | - | Folder scanned for epub files |
| `CONVERTER_OUT` | - | Folder the audiobooks are written to |
| `CONVERTER_SPEED` | `1.0` | Speech rate, 0.5 - 2.0 (pitch-preserving `rate` request field) |
| `CONVERTER_BITRATE` | `192k` | Bitrate of the `.m4b` audio |
| `CONVERTER_MAX_DEPTH` | `3` | How deep below `CONVERTER_IN` epubs are looked for (`-1` = unlimited) |
| `CONVERTER_MIN_TEXT_CHARS` | `1000` | Text a document without any title needs to count as the continuation of the previous chapter |
| `CONVERTER_EXTRA_SKIP_PATTERNS` | - | Comma separated regexps, matched against chapter titles, dropped in addition to the built-in list |
| `CONVERTER_KEEP_PATTERNS` | - | Comma separated regexps that override both lists, so a real chapter called "Index of Translated Works" survives |
| `CONVERTER_POLL_MINUTES` | `60` | Wait between scans |
| `CONVERTER_MAX_ATTEMPTS` | `3` | Conversion attempts per book before giving up (until the next restart) |

Escaping in the pattern lists: write `\,` for a comma inside a regexp.

### Speech engine

| Variable | Default | Meaning |
| --- | --- | --- |
| `CONVERTER_TTS_BIN` | `qwen_tts` | The engine executable (a bare name must be on the `PATH`) |
| `CONVERTER_TTS_MODEL` | user cache | Model directory with `config.json`; when it does not hold a model yet, `TTS_MODEL_ID` is downloaded into it (default `~/.cache/qwen3-tts/<model>`) |
| `CONVERTER_TTS_MODEL_ID` | `Qwen/Qwen3-TTS-12Hz-0.6B-CustomVoice` | Hugging Face repository fetched on first use; `0.6B-CustomVoice`, `1.7B-CustomVoice`, `1.7B-VoiceDesign`, `0.6B-Base`, `1.7B-Base` all work |
| `CONVERTER_TTS_URL` | - | Use a running engine server (`http://host:port`) instead of starting one |
| `CONVERTER_TTS_SPEAKER` | `ryan` | Narrator voice (`qwen_tts -d <model> --list-speakers`) |
| `CONVERTER_TTS_QUANTIZE` | `int8` | `int8` for a faster, smaller talker, `mixed-cpu` for the mixed-precision CPU build, empty for full precision |
| `CONVERTER_TTS_MAX_CHARS` | `600` | Text budget per engine request, 120 - 1792; segmentation never splits a sentence or a quotation to honour it |
| `CONVERTER_TTS_SILENCE_MS` | `500` | Quiet inserted between segments inside a chapter, 0 - 3000 |
| `CONVERTER_TTS_THREADS` | `0` | Engine threads, `0` = engine default |
| `CONVERTER_TTS_SEED` | `-1` | `>= 0` makes synthesis reproducible; `-1` lets the engine pick |
| `CONVERTER_TTS_LANGUAGE` | `auto` | `auto`, `German` or `English` |
| `CONVERTER_TTS_STARTUP_SECONDS` | `300` | Budget for the engine server to load the model |

**Engine lifecycle.** The converter starts `qwen_tts --serve` as a child
process, waits for its health check and stops it on exit - the weights load
once per run, not once per chapter. If the server dies between segments the
converter restarts it and retries the segment; books that are already done keep
their audio. With `CONVERTER_TTS_URL` nothing is started and the server at that
address is used as is (health-checked before every book); the binary and model
options then refer to that machine and are not checked locally.

**Language.** With `auto` the converter takes `dc:language` from the epub; if
that is missing it decides from the text (German and English markers). A book
whose metadata says one thing and whose text says another is narrated in the
language of the text once the sample is decisive - the log always says which
source decided.

Each chapter is split into segments of at most `TTS_MAX_CHARS` characters.
Every segment is synthesized with the same voice, rate and seed, so the chapter
sounds like one recording; the configured silence fills the gaps.

## Requirements

`ffmpeg` and `ffprobe` must be on the `PATH`, plus the engine: the `qwen_tts`
executable (build it from the
[engine repository](https://github.com/gabriele-mastrapasqua/qwen3-tts) with
`make qwen_tts`, it needs gcc and OpenBLAS headers). The model weights are
fetched from Hugging Face automatically on the first run (~1.1 GB for the
0.6 B model into `~/.cache/qwen3-tts/`, resumable) - afterwards the whole
pipeline runs offline. If the engine was built against a private OpenBLAS,
start the converter with that library on `LD_LIBRARY_PATH`, the child
processes inherit it.

Model sizes: the 0.6 B model is the practical choice on a CPU-only machine
(int8 measures at RTF ~1.1 on this laptop, a 10-hour novel converts
overnight), the 1.7 B model is markedly better and the
only one with the full emotion/instruct stack, but costs about twice the time
per chapter. See `BENCHMARKS.md` for the measured numbers.

## Trying it: the demo book

`demo/` generates a small original German story as a proper epub - three
chapters with narration and dialogue, enough text to exercise segmentation:

```bash
go build -o audiobookconverter .
go run ./demo /tmp/books/demo-deutsch.epub

CONVERTER_IN=/tmp/books CONVERTER_OUT=/tmp/audiobooks ./audiobookconverter
```

The log shows the chapter selection, the detected language and per-chapter
segment counts; the result is `demo-deutsch/demo-deutsch.m4b` once the epub
marker appears next to it.

## Docker

The image builds the converter and the engine from source; the model weights
are never part of the image. Give the container a volume for the user cache
and the first run downloads the model into it (~1.1 GB, needs internet; every
later run is offline):

```bash
docker build -t audiobookconverter .
docker run -d --name audiobookconverter \
  -v /path/to/books:/books:ro \
  -v /path/to/audiobooks:/audiobooks \
  -v audiobookconverter-models:/root/.cache/qwen3-tts \
  -e CONVERTER_IN=/books -e CONVERTER_OUT=/audiobooks \
  audiobookconverter
```

An already downloaded model folder can be mounted anywhere and selected with
`CONVERTER_TTS_MODEL` (or `CONVERTER_TTS_URL` points at an engine server
elsewhere).

The engine image pins the engine commit: the wrapper speaks this exact server
contract, so an upstream change is only taken after the test suite has been
run against it.

### One book at a time

`convert-epub.sh` starts a container for the given epubs instead of watching a
folder, shows the conversion log, and exits when every book is done:

```bash
./convert-epub.sh ~/Books/"The Secret Name.epub"
./convert-epub.sh -o ~/Audiobooks --speaker vivian --bitrate 128k ~/Books/*.epub
ABC_MODEL_DIR=~/models/qwen3-tts-0.6b ./convert-epub.sh book.epub  # existing model folder
```

The audiobooks appear in the output folder (`.` by default), one subfolder per
book. A book whose folder in the output already holds its epub is reported as
converted and left alone. `--help` lists the options, `--dry-run` prints the
`docker run` command instead of running it, `--build` builds the image first.
The files are written by root inside the container, so they may be owned by
root on the host.

## Development

```bash
go build .
go test ./...
```

The test suite covers the chapter selection (rules plus the shipped reference
book), the language detection, the segmenter, and the audio pipeline end to
end against a fake engine server, so no model download is needed. The tests
that assemble audio need `ffmpeg` and skip themselves without it.
