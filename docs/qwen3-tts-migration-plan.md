# Replacing audiblez with Qwen3-TTS

Status: M0 implemented, and audiblez is gone. The Qwen3-TTS pipeline is the only
engine - there is no `CONVERTER_ENGINE` switch any more. The converter starts the
engine's HTTP server as a child process (`qwen_tts --serve`), keeps the weights
resident for the whole run, and talks `/v1/tts` to it; `CONVERTER_TTS_URL` points
at a server someone else runs. The multi-voice and director stages below are
still proposals. Numbers marked *est.* were not measured on our hardware; the
measured ones are in `BENCHMARKS.md`.

Goal: keep the automatic epub pipeline, drop audiblez (Edge-TTS, cloud, one flat
voice), and gain English + German novels with per-character voices and a narrator
that adapts to the scene - running locally.

## 1. What each piece can do today

### The wrapper's share of the pipeline

Everything except the synthesis itself is engine-independent and stays:

| Component | File | Fate |
| --- | --- | --- |
| folder watch, retry, finished-marker, depth scan | `main.go` | unchanged |
| epub parsing, chapter selection, title/skip/keep logic | `epub.go` | unchanged |
| spoken-text extraction from the chapter XHTML (`analyseHTML`, audiblez's tag set) | `epub.go` | unchanged - already gives one block per paragraph, `\n`-joined, per chapter |
| `runAudiblez` (exec + stdout progress parsing) | `audio.go` | replaced |
| `collectChapters` wav-name regex pairing to audiblez | `audio.go` | replaced by naming we control |
| `buildM4B` (concat demuxer + ffmeta chapters, aac) | `audio.go` | unchanged - never depended on audiblez's encoder |
| derived chapters-only epub | `epub.go` `writeDerivedEpub` | obsolete - its only consumer was audiblez |
| Docker audiblez/piper install | `Dockerfile` | replaced by a TTS engine image |

Key point: the wrapper already owns chapter selection and text extraction.
audiblez only ever received an epub whose chapters we picked and whose text we
could have read ourselves. Replacing it is a synthesis-backend swap, not a redesign.

### Qwen3-TTS (official, Apache-2.0)

- Models 0.6B / 1.7B: `CustomVoice` (9 preset timbres + natural-language
  `instruct` style control, 1.7B only), `VoiceDesign` (any voice from a text
  description, 1.7B only), `Base` (3-second voice clone, no instruction control).
- 10 languages including English and German; per-request `language` field.
- Claim: context-aware adaptation of tone/rate/emotion from text semantics;
  robust to noisy text.
- Official `qwen-tts` Python package targets CUDA + flash-attention. Measured
  CPU performance of the PyTorch path is unusable: a third party measured a
  short sentence (~7 s audio) at 289 s warm / 559 s cold on an i7-12700K CPU,
  i.e. RTF ~45-85. For a GPU-free host it is not an option.

### The enabler: pure-C Qwen3-TTS engine (gabriele-mastrapasqua/qwen3-tts, MIT)

Same model weights (mmap'd safetensors), runs on CPU without Python/PyTorch, and
it carries the features the official docs promise *plus* audiobook-specific ones:

- **Preset voices, `instruct` (1.7B), `--emotion`** - steering-vector based,
  works in all 10 languages (the repo ships German emotion samples, e.g.
  vivian/anger); 6 primaries + 7 blended dyads (`nostalgia`, `dread`-like
  `despair`, `contempt`, ...).
- **Inline markup built for audiobooks**: `[sad]`, `[joy]`, ... switch emotion
  sentence-by-sentence inside one text, `[pause:400ms]`/`[break:1s]` insert gaps,
  `[sigh]`/`[laugh]` paralinguistics (alpha). Server auto-detects and renders
  span-by-span.
- **Voice cloning via portable profiles**: clone from a reference clip once
  (Base model), save as `.qvoice` graft (~25 MB) or 8 KB x-vector `.bin`, then
  synthesize on CustomVoice with the clone. Crucially, on the graft path the
  **emotion levers keep working on cloned voices** - the official Base model has
  no instruct, the C engine restores emotion for clones.
- **0.6B is expressive too**: presets and cloned voices emote from shipped
  4 KB direction assets, no extra download; 1.7B has the full steering stack.
- **HTTP server** `--serve`: `/v1/tts`, `/v1/tts/stream`, OpenAI-compatible
  `/v1/audio/speech`, `GET /v1/speakers`, `GET /v1/health`. Per-request JSON
  takes `text, speaker, language, instruct, emotion, volume, rate, seed,
  temperature, ...`; reproducible with `seed`. One server = one model.
- **Limits that shape the design**: clone selection is fixed *at server start*
  (no per-request clone field) - per-request switching works for the 9 presets;
  server text limit defaults to ~1 800 chars (per-request cap); segments must be
  split below that anyway for prosody stability.
- Performance (vendor-measured): 0.6B int8 **sub-realtime on an M1 CPU**
  (RTF 0.69-0.93 incl. server); 1.7B int8 ~1.8 on M1; x86 hot paths have
  AVX-512/VNNI kernels. Optional CUDA backend: 1.7B quant-mixed RTF ~0.44 on
  RTX 4060-class, but its serving path is explicitly unqualified (batch/CLI use
  is fine).

### Target hardware (this host)

i7-1165G7 (4C/8T TigerLake-U), 15 GB RAM, no NVIDIA GPU (Iris Xe only),
AVX-512 + VNNI present - the engine's best x86 kernel path, but a
memory-bandwidth-starved laptop. Plausible outcome: **0.6B int8 at RTF ~2-3
(est.)**, usable for overnight batch conversion; **1.7B too slow for whole
novels** on this box. M0 measures it with the engine's own `make bench-matrix`.

## 2. Throughput reality check

A 12 h audiobook ≈ 720 min of audio.

| Engine | RTF | Book time (est.) | Notes |
| --- | --- | --- | --- |
| audiblez / Edge-TTS (today) | network | ~1-2 h | cloud, flat voice |
| C engine 0.6B int8, M1 | 0.7-0.9 | ~9-11 h | vendor data |
| C engine 0.6B int8, i7-1165G7 | ~2-3 est. | **~24-36 h** | M0 measures |
| C engine 1.7B int8, this host | ~5-8 est. | 2.5-4 d | probably impractical |
| C engine 1.7B, remote RTX 4060 | ~0.45 | ~5.5 h | if a GPU box exists |

Plus director overhead (if LLM-based, see §4): well under the TTS cost.

Consequence: local Qwen is a **batch** story - a book per night or weekend, not
per hour. If that is acceptable, proceed; if not, the only fast-and-expressive
option is a GPU host (same engine, different RTF).

## 3. Proposed architecture

```
watcher (main.go, unchanged)
  └─ epub.go: select chapters, extract paragraph text     (unchanged)
       └─ script compiler (new): paragraphs -> segments
            ├─ splitter: sentence-grouping, <= ~1200 chars
            ├─ speaker/quote detection (EN + DE quote styles)
            ├─ casting: segment -> voice profile (preset or .bin/.qvoice)
            └─ mood: inline [emotion]/[pause] tags
                 └─ synthesizer (new interface): segment -> 24 kHz wav
                      ├─ exec qwen_tts CLI (per voice profile)
                      └─ or HTTP /v1/tts (narrator server)
                      └─ segment cache (content-addressed)
                           └─ chapter assembler: segment wavs + gaps -> chapter_N.wav
                                └─ buildM4B (unchanged) -> <Book>.m4b + markers
```

### Segments, not chapters, become the unit of synthesis

`Segment{Text, Language, Voice, Emotion, Instruct, Rate, PauseAfterMs}`.

- The engine's inline `[mood]`/`[pause]` markup means one request can already
  carry several narration sentences with different moods; a speaker change
  cannot happen inside a request (one voice per request). So the compiler emits
  **one request per speaker run**, with mood tags inside it.
- Gaps between segments (silence via ffmpeg or pre-inserted) provide paragraph
  and scene pacing; `[pause:400ms]`/`[break:1s]` handle within-segment beats.
- Chapter audio = concat of its segment wavs → `chapter_01.wav`, so
  `collectChapters`/`buildM4B` keep working with names we control (the
  `derivedWavRE` audiblez-name matching disappears).
- Progress reporting becomes honest: done-segments/total per chapter, no
  stdout scraping; `conversionStatus` shrinks.

### Casting (character voices)

- **Cast file per book** (`casting.json`, stored with the converted book or a
  sidecar in the work cache): character → voice reference. Deterministic
  re-casts; the LLM/heuristics propose, a user-provided cast file overrides.
- Tier 1 (M2): preset pool only - male/female/young/old picks from the 9
  timbres. Free, fast, but EN/Asian-native timbres for German text (see §5).
- Tier 2 (M3): per-book designed cast - VoiceDesign renders a short reference
  per character from a description, Base turns it into a `.qvoice` graft
  (one-time, ~seconds each); every later line uses the graft and keeps emotion.
  Book-specific voices, real differentiation; needs the Base model next to the
  CustomVoice model (or the CLI generates clips with Base and grafts via the
  provided `qvoice_to_graft`/`qvoice_to_xvec` helpers).
- Reproducibility: engine takes `--seed`; cast + seeds recorded per book.

### The script compiler (the genuinely new intelligence)

Three increasing tiers, so the pipeline works before the hard part is solved:

1. **Rules (M1/M2)**: paragraph boundaries already exist in `epubChapter.Text`.
   Quote extraction with configurable quote pairs - `"..."`/`'...'` (EN),
   `„..."`/`‚...` and `»...«` (DE), and dash-style dialogue lines common in
   German fiction - auto-detected per book by frequency. Speaker alternation by
   order within a paragraph run, said-verb tags (`sagte`, `said`, `fragte`,
   `murmelte`) attributed to the adjacent speaker. Narrator mood keywords →
   `[emotion]` tags (`flüsterte` → calm, `schrie` → anger...).
   Limitation (honest): multi-character scenes will get the wrong speaker now
   and then.
2. **LLM director (M3)**: an OpenAI-compatible endpoint (Ollama/llama.cpp
   locally, or any API) receives a scene window + current cast and returns the
   tagged script as JSON: speaker, gender/age for unseen characters, emotion per
   line, narrator mood per paragraph. This is the only component that can do
   novel-level dialogue attribution well. Token math is small next to TTS:
   a 110k-word novel is ~150k input tokens, ~300-450k with scene-window
   overlap; a 7-14 B model at ~20-60 prompt-tok/s on this CPU *est.* is well
   under half a day, far below the 24 h+ synthesis.
3. **Manual**: user-provided annotated/cast file always wins.

### Engine invocation

HTTP server has one voice-profile-at-start limitation, so:

- **CLI mode (M1)**: Go execs `qwen_tts -d <model> [--load-voice x.bin --icl-only]
  -l German -s vivian --emotion calm --rate 1.0 --seed N --text ... -o seg.wav`.
  Weights are mmap'd and survive in the page cache across invocations, so the
  per-segment process cost is tokenizer + warm-up (~1 s *est.*). Matches the
  current exec-audiblez architecture, switches voices per call, trivially
  testable with a fake binary.
- **Hybrid server mode (later, if per-segment overhead hurts)**: warm server
  serves narrator requests (presets switchable per request); character clones
  either via per-profile server pool (weights shared between processes via
  mmap) or stay on CLI. Decide from M0/M1 measurements.
- Config: `CONVERTER_TTS_BIN|CONVERTER_TTS_URL`, `CONVERTER_TTS_MODEL_DIR`,
  `CONVERTER_MODEL=0.6b|1.7b`, `CONVERTER_NARRATOR_F/M`, `CONVERTER_CAST_FILE`,
  `CONVERTER_DIRECTOR=off|rules|llm`, `CONVERTER_LLM_URL/MODEL`,
  `CONVERTER_SPEED` (maps to `--rate`/`rate`), existing `CONVERTER_VOICE` becomes
  the narrator preset.

### Docker topology

- `tts` image: small Debian build of the C engine (OpenBLAS), models pulled into
  a named volume (`audiobookconverter-models`), `--serve` optional.
- Converter image: Go binary, no Python at all (audiblez, torch, espeak-ng go
  away). Talks to the engine via local exec (shared volume) or HTTP.
- `convert-epub.sh`: starts both containers, one book at a time, same UX.
- Cache volume now holds model weights (~3 GB int8 for 0.6B; +Base ~3 GB for
  cloning; 1.7B ~8 GB bf16 or quantized) instead of piper voices.

## 4. Language handling (EN + DE)

- Book language from `<dc:language>`, fallback heuristic (stopwords/umlauts);
  passed per request as `language`. Mixed books get per-segment detection.
- German quality: Qwen3-TTS-12Hz-1.7B WER 1.235 on the DE test set (0.960 for
  the 25Hz-1.7B variant; ElevenLabs is 0.572 and remains the cloud benchmark).
  Intelligibility is not the risk; **accent/idiom of non-native preset timbres
  in German is** - there is no German-native preset (Ryan/Aiden are EN-native,
  the rest ZH/JA/KO). Mitigations, in order of effort: listen-test all presets
  with DE prose (M0); designed/cloned German narrator (Tier 2 casting); user
  clone of a rights-cleared recording.
- DE typography needs explicit test cases: `„..."`, `»...«`, ß vs. ss in
  epubs, numbers/dates (`1848`, `3,50 €`, `der Dreißigjährige`), French names in
  German text. audiblez/piper had a German text frontend; Qwen claims noisy-text
  robustness, but the M0 fixture must contain all of it.
- Emotion assets: preset steering ships in the repo (DE samples exist); cloned
  voices on 1.7B want a per-language `.expr` pack - availability/quality for
  German is an open point (M0 checks the HF asset pack).

## 5. Risks

| Risk | Impact | Mitigation |
| --- | --- | --- |
| RTF on this laptop higher than est. | a book takes a weekend | M0 measures before any code |
| Wrong speaker attribution (rules tier) | comedic/wrong voice acting | M3 LLM director; cast file override; rules ship conservative (unattributed quotes stay narrator) |
| German preset accent | narrator sounds off | designed/cloned German voice (Tier 2) |
| Emotion system is beta (vendor's word) | over/under-driven lines | emotion optional per book; per-line `--instruct` library; seeds recorded so a bad line regenerates identically or is fixed in the script file |
| Per-segment prosody seams | audible level jumps | one request per speaker run (engine-internal span concat is seam-tuned); short segments only where voice changes |
| Long-book consistency | character voice drift across chapters | persisted cast + profiles + seeds; profiles are data, not state |
| Engine is a third-party C project | API/behaviour changes | pin a release + commit hash, same policy as `audiblez==0.4.9`; fixtures catch wav-level regressions |
| Model weights licensing | distribution | Apache-2.0 weights downloaded at runtime into a volume, never baked into the image (as today) |

## 6. Milestones

- **M0 - Benchmarks & listening gate (no product code).** Build the C engine on
  this host, `--caps` (expect AVX-512/VNNI), `make bench-matrix` for 0.6B
  int8/1.7B; listening set: EN + DE narration paragraphs, preset comparison,
  `--emotion` sweep incl. DE, one clone, one VoiceDesign German narrator.
  Decision: model size, narrator voice, proceed-or-stop on throughput.
- **M1 - Engine swap, single voice.** `synthesizer` interface + CLI engine;
  Go-side sentence-group segmentation (≤ ~1200 chars); chapter wavs named by us;
  drop `runAudiblez`, `derivedWavRE`, `writeDerivedEpub`, audiblez install in
  Docker. Config/env updated. Tests: fake engine binary + fixture epubs
  (EN + DE), m4b regression.
- **M2 - Rules director + casting.** Quote/speaker extraction (EN+DE styles),
  preset cast pool, `casting.json`, gaps/pacing, per-segment language.
  Acceptance: fixture novel renders with ≥ 2 distinct voices, attribution errors
  counted in the test fixture report.
- **M3 - Drama + LLM director.** OpenAI-compatible director behind a flag
  (speaker/emotion/mood JSON), inline `[mood]`/`[pause]` composition, Tier-2
  designed casts (VoiceDesign→Base→graft pipeline, profile cache).
- **M4 - Ops.** Docker split (converter + tts), model volume, segment cache +
  resume-mid-book, `convert-epub.sh` flags, README rewrite.

## 7. Open points for discussion

1. **Throughput acceptance**: is ~1 book/night (0.6B int8 on this laptop, M0
   number pending) acceptable, or should the architecture assume a GPU host
   (RTX 4060-class → ~5 h/book with 1.7B)? *Recommendation: assume CPU-laptop,
   design the `synthesizer` interface so a remote engine is just a URL.*
2. **Model size**: 0.6B int8 (fits, fast, emotion on presets+clones, no free-form
   `instruct`) vs 1.7B (full instruct, better quality, slow here). Related: does
   the whole-book quality gap justify a GPU? *Recommendation: 0.6B default on
   this host, 1.7B configurable for GPU hosts.*
3. **Narrator voice for German**: accept an EN-native preset's accent, design a
   German narrator via VoiceDesign, or clone a recording the user provides
   (rights must be theirs)? *Recommendation: M0 listening test decides; likely a
   designed/cloned German narrator.*
4. **Speaker attribution policy before the LLM exists**: rules with
   under-attribution bias (unmatched quotes narrated flat) or skip
   multi-character dialogue entirely from casting? How many wrong attributions
   per scene are tolerable before M3 becomes mandatory?
5. **LLM director dependency**: local only (Ollama/llama.cpp, which model —
   Qwen3-8B? multilingual, but a few hours of CPU) or also remote API
   (privacy tradeoff, fast)? Should it run per chapter (cheap, no global cast
   memory) or per book with a persisted character register (better consistency)?
6. **Cast policy**: auto-cast every named speaker, or only speakers above an
   appearance threshold (background "said a man" voices stay narrator)? Should a
   user-provided `casting.json` be able to assign cloned reference audio per
   character?
7. **Emotion scope**: emotion only on dialogue + strongly-marked narration vs
   mood tags on every paragraph? More tags = more render time (per-span engine
   work) and more risk of soap-opera delivery. *Recommendation: dialogue +
   keyword-flagged narration; `[pause]` at scene breaks.*
8. **audiblez removal**: hard cutover (clean, per this plan) or keep audiblez as
   a selectable second engine during transition? *Recommendation: cutover; the
   git history is the fallback.*
9. **Audio format**: output is 24 kHz mono; keep `CONVERTER_BITRATE=192k` (sized
   for stereo sources) or drop to ~96k? Loudness normalize per segment
   (ffmpeg `loudnorm`) since emotion changes volume?
10. **Resume/checkpoint**: segment cache content-addressed (`sha256(text|voice|
    emotion|lang|rate|model|seed)`) in the existing model volume or a scratch
    volume? Needed for multi-day books on a laptop that sleeps.
11. **Footnotes/inline references**: piper-era books carry inline footnote
    markers; suppress (`<sup>` text) or speak? audiblez spoke them; an improvement
    might be in scope or might be a separate issue.
12. **Speed control**: `rate` is pitch-preserving per request; confirm it composes
    with emotion recipes before wiring `CONVERTER_SPEED`.

## Appendix: evidence links

- Qwen3-TTS models, languages, instruct/clone/design APIs:
  github.com/QwenLM/Qwen3-TTS (Apache-2.0); DE WER from its eval tables.
- C engine (MIT): github.com/gabriele-mastrapasqua/qwen3-tts — RTF tables,
  emotion/`[markup]` docs, `.qvoice` cloning, `docs/serving/api.md`.
- PyTorch-CPU slowness: note.com GGUF measurements (RTF ~19-29 on desktop
  i7s with cpp, PyTorch CPU "several minutes" per sentence);
  fulloch.com Qwen3-TTS ONNX post; ocdevel.com int8 CPU benchmarks
  (RTF ~3 on 4 vCPU, ~3.2 GB RAM, int4 slower than int8).
