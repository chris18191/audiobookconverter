# Benchmarks

Measured on this machine (i7-1165G7, 4 cores / 8 threads, 15 GB RAM, no GPU),
engine commit `ef339be`, model `qwen3-tts-0.6b`, one resident `--serve`
process, `OMP_NUM_THREADS=4`. Three identical ~360-character requests per
mode after one warm-up request; RTF = wall time / audio time.

| Engine mode | Model load | RTF |
| --- | --- | --- |
| full precision | 7.0 s | 1.78 |
| `--int8` | 4.7 s | **1.12** |
| `--quant-mixed-cpu` | 4.5 s | 1.51 |

Conclusions:

- `--int8` is the default (`CONVERTER_TTS_QUANTIZE=int8`): fastest here, and
  `--quant-mixed-cpu` (int8 talker + int4 code predictor) does not pay off on
  the 0.6 B model even though the engine advertises it as the best CPU quant -
  that ranking is for the 1.7 B model.
- With a resident server the model loads once per converter run, not once per
  segment, and the per-request overhead is a prefix-cache hit (repeated
  chapter prompts reuse positions). At RTF ~1.1 a CPU converts a 10-hour novel
  overnight rather than over a week.
- Not measured: the 1.7 B model. Vendor figures put it at about 1.6x the
  0.6 B time (RTF ~1.8 on an M1), so expect RTF 2-3 here - still batch
  material, and it is the model with the full emotion/instruct stack.
