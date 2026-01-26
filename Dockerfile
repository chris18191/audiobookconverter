FROM python:3.12-slim-bookworm

RUN apt update
RUN apt install -y ffmpeg espeak-ng
RUN pip install audiblez 
RUN apt install -y golang
RUN apt install -y wget

WORKDIR /converter
COPY . /converter
RUN wget https://go.dev/dl/go1.25.6.linux-amd64.tar.gz
RUN rm -rf /usr/local/go && tar -C /usr/local -xzf go1.25.6.linux-amd64.tar.gz
RUN go install golang.org/dl/go1.25.6@latest
RUN /usr/local/go/bin/go mod tidy
RUN /usr/local/go/bin/go build .
RUN cp /converter/audiobookconverter /opt/
WORKDIR /opt/
RUN rm -rf /converter

ENV FOLDER_IN=/books
ENV FOLDER_OUT=/audiobooks

CMD /opt/audiobookconverter
