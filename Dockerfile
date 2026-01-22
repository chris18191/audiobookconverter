FROM golang:1.25.6-alpine AS builder

COPY . /converter
WORKDIR /converter
RUN go build .

FROM python:3.12-slim-bookworm

COPY --from=builder /converter /converter

RUN apt update
RUN apt install -y ffmpeg espeak-ng
RUN pip install audiblez 

ENV FOLDER_IN=/books
ENV FOLDER_OUT=/audiobooks

CMD /converter/audiobookconverter
