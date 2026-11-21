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
FROM debian:bookworm-slim AS engine
RUN apt-get update \
    && apt-get install -y --no-install-recommends build-essential git ca-certificates libopenblas-dev \
    && rm -rf /var/lib/apt/lists/*
ARG QWEN_TTS_COMMIT=ef339be58a778b062e1c14382347964552eae007
RUN git clone https://github.com/gabriele-mastrapasqua/qwen3-tts /engine \
    && cd /engine \
    && git checkout "$QWEN_TTS_COMMIT" \
    && make -j"$(nproc)" qwen_tts

FROM debian:bookworm-slim
# ffmpeg assembles the chapters; the engine needs its BLAS runtime.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg libopenblas0 ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=engine /engine/qwen_tts /usr/local/bin/qwen_tts
COPY --from=converter /audiobookconverter /opt/

ENV FOLDER_IN=/books
ENV FOLDER_OUT=/audiobooks
# No CONVERTER_TTS_MODEL here: the converter downloads the model into the user
# cache (/root/.cache/qwen3-tts) on first use. Mount a volume there to keep
# the weights across containers, or set CONVERTER_TTS_MODEL + -v to an existing
# model directory (see convert-epub.sh --model).

CMD ["/opt/audiobookconverter"]
