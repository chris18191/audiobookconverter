# The converter is built with Go; the speech engine is built from source with
# gcc. Both are dropped into a slim runtime that only adds ffmpeg.
FROM golang:1.26-bookworm AS converter
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /audiobookconverter .

# Pure C Qwen3-TTS engine. The commit is pinned: the wrapper talks HTTP to this
# exact server contract, and an unannounced change upstream turns into failed
# conversions. Bumping it is a deliberate commit, checked against the test
# suite before the image takes it.
# SIMD=portable (AVX2+FMA, any x86 since 2013): without it the engine detects
# the BUILD machine's ISA and the image SIGILLs on weaker CPUs at startup.
# For a machine without even AVX2 build with SIMD=scalar.
FROM debian:bookworm-slim AS engine
RUN apt-get update \
    && apt-get install -y --no-install-recommends build-essential git ca-certificates libopenblas-dev \
    && rm -rf /var/lib/apt/lists/*
ARG QWEN_TTS_COMMIT=ef339be58a778b062e1c14382347964552eae007
RUN git clone https://github.com/gabriele-mastrapasqua/qwen3-tts /engine \
    && cd /engine \
    && git checkout "$QWEN_TTS_COMMIT" \
    && make -j"$(nproc)" qwen_tts SIMD=portable

# The default speech model ships inside the image (~2.4 GB, fetched from
# Hugging Face at build time). The ARG allows rebuilds with a different
# repository - the converter's own allowlist in model.go lists the ones the
# engine can run.
FROM debian:bookworm-slim AS model
RUN apt-get update \
    && apt-get install -y --no-install-recommends curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*
ARG QWEN_TTS_MODEL_ID=Qwen/Qwen3-TTS-12Hz-0.6B-CustomVoice
RUN set -eu; mkdir -p /model/speech_tokenizer; \
	for f in config.json generation_config.json tokenizer_config.json \
	         preprocessor_config.json model.safetensors vocab.json merges.txt \
	         speech_tokenizer/config.json speech_tokenizer/configuration.json \
	         speech_tokenizer/model.safetensors speech_tokenizer/preprocessor_config.json; do \
		curl -fL --retry 3 -o "/model/$f" \
			"https://huggingface.co/${QWEN_TTS_MODEL_ID}/resolve/main/$f"; \
	done

FROM debian:bookworm-slim
# ffmpeg assembles the chapters; the engine needs its BLAS runtime.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg libopenblas0 ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=engine /engine/qwen_tts /usr/local/bin/qwen_tts
COPY --from=model /model /opt/models/qwen3-tts-0.6b
COPY --from=converter /audiobookconverter /opt/

ENV FOLDER_IN=/books
ENV FOLDER_OUT=/audiobooks
# The baked 0.6 B model answers CONVERTER_TTS_MODEL out of the box; mounting
# another model directory over it (see convert-epub.sh --model) overrides, and
# because the directory is complete the converter never downloads.
ENV CONVERTER_TTS_MODEL=/opt/models/qwen3-tts-0.6b

CMD ["/opt/audiobookconverter"]
