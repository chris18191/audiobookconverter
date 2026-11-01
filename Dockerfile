FROM python:3.12-slim-bookworm

RUN apt update \
    && apt install -y --no-install-recommends ffmpeg espeak-ng wget \
    && rm -rf /var/lib/apt/lists/*
# audiblez pulls in torch, and the default PyPI torch wheel drags the whole CUDA
# stack (cublas, cudnn, nccl, triton: several gigabytes) into the image even on
# a CPU only host. The CPU build of torch is installed first so that the CUDA
# wheels are never considered. The voice model dependencies are large; without a
# generous timeout the build dies on a slow mirror.
# The audiblez version is pinned: the wrapper pairs audiblez's wav files with the
# chapters by their file names, so an unannounced change upstream turns into
# mislabelled or missing audio. Upgrading is a deliberate commit.
RUN pip install --no-cache-dir --timeout 180 --retries 5 \
        --index-url https://download.pytorch.org/whl/cpu torch \
    && pip install --no-cache-dir --timeout 180 --retries 5 audiblez==0.4.9

WORKDIR /converter
COPY go.mod go.sum ./
RUN wget -q https://go.dev/dl/go1.25.6.linux-amd64.tar.gz \
    && rm -rf /usr/local/go && tar -C /usr/local -xzf go1.25.6.linux-amd64.tar.gz \
    && rm go1.25.6.linux-amd64.tar.gz
RUN /usr/local/go/bin/go mod download
COPY . .
RUN /usr/local/go/bin/go build .
RUN mv /converter/audiobookconverter /opt/
WORKDIR /opt/
RUN rm -rf /converter

ENV FOLDER_IN=/books
ENV FOLDER_OUT=/audiobooks

CMD ["/opt/audiobookconverter"]
