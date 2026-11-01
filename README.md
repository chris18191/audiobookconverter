# Audiobook Converter

A wrapper around [audiblez](https://github.com/santinic/audiblez) that watches an
input folder, converts the EPUBs it finds into audiobooks, and writes the result
to an output folder.

audiblez turns every readable document of an epub into speech. Left alone that
means a book full of cover pages, tables of contents, copyright notices,
glossaries and bibliographies - and audio files named `chapter_5_bm_lewis_OEBPS_c9H`.
This wrapper therefore selects the chapters itself and names the audio after them.

## What it does

For each `<Book>.epub` in the input folder:

1. **Read the epub** - the OPF spine gives the reading order, the table of
   contents (EPUB3 `nav` or EPUB2 `toc.ncx`) gives the chapter titles, the
   first `<h1>`-`<h4>` of a document is the fallback title.
2. **Keep only readable chapters.** Dropped: tables of contents (including the
   generated `Contents` pages calibre emits), documents labelled with a
   non-content `epub:type` (`page-list`, `bibliography`, `index`, `colophon`,
   `cover`, ...), documents whose title matches a back matter pattern
   (bibliography, glossary, acknowledgments, about the author, ...), and front or
   back matter too short to be a chapter. Everything audiblez needs to keep -
   the reading order and the text - is untouched.
3. **Build a chapters-only epub** (`chapter_01.xhtml`, `chapter_02.xhtml`, ...
   in spine order, with the original metadata and cover, and without the images,
   which are irrelevant for speech synthesis).
4. **Run audiblez** on it, so the speech synthesis order is the chapter order by
   construction.
5. **Write one mp3 per chapter**, named `<number> - <chapter title>.mp3`, e.g.
   `4 - 1. Origins.mp3`, and one `<Book>.m4b` holding the whole book with the
   epub chapter titles as chapter markers.
6. **Copy the source epub** into the book's folder in the output directory. That
   copy is the marker of a finished book: the scanner never picks it up again as
   new work.

```
<out>/The Secret Name/
├── 1 - An Antlered Boy.mp3
├── 2 - A Visit to the Fair.mp3
├── ...
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
| `CONVERTER_VOICE` | `bm_lewis` | audiblez voice (`audiblez --help` lists them) |
| `CONVERTER_SPEED` | `1.0` | Speech speed, 0.5 - 2.0 |
| `CONVERTER_BITRATE` | `192k` | Bitrate of the mp3 and m4b audio |
| `CONVERTER_M4B` | `true` | Also write the single file `.m4b` |
| `CONVERTER_MAX_DEPTH` | `3` | How deep below `CONVERTER_IN` epubs are looked for (`-1` = unlimited) |
| `CONVERTER_MIN_TEXT_CHARS` | `1000` | Text a document without any title needs to count as the continuation of the previous chapter |
| `CONVERTER_EXTRA_SKIP_PATTERNS` | - | Comma separated regexps, matched against chapter titles, dropped in addition to the built-in list |
| `CONVERTER_KEEP_PATTERNS` | - | Comma separated regexps that override both lists, so a real chapter called "Index of Translated Works" survives |
| `CONVERTER_POLL_MINUTES` | `60` | Wait between scans |
| `CONVERTER_MAX_ATTEMPTS` | `3` | Conversion attempts per book before giving up (until the next restart) |

Escaping in the pattern lists: write `\,` for a comma inside a regexp.

## Requirements

`audiblez`, `ffmpeg` and `ffprobe` must be on the `PATH`. The first conversion
downloads the voice model of the configured voice.

## Docker

```bash
docker build -t audiobookconverter .
docker run -d --name audiobookconverter \
  -v /path/to/books:/books \
  -v /path/to/audiobooks:/audiobooks \
  -v audiobookconverter-cache:/root/.cache \
  -e CONVERTER_IN=/books -e CONVERTER_OUT=/audiobooks \
  -e CONVERTER_VOICE=bm_lewis \
  audiobookconverter
```

The cache volume keeps the voice model across container restarts.

## Development

```bash
go build .
go test ./...
```

The test suite covers the chapter selection (rules plus the shipped reference
book), the derived epub, and the audio pipeline end to end against a stub of
audiblez, so no voice model download is needed.
