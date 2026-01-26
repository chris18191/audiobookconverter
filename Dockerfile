FROM python:3.12-slim-bookworm

RUN apt update
RUN apt install -y ffmpeg espeak-ng
RUN pip install audiblez 
RUN apt install -y golang

WORKDIR /converter
COPY . /converter
RUN go install golang.org/dl/go1.25.6@latest
RUN go build .
RUN ls
RUN cp /converter/audiobookconverter /opt/
WORKDIR /opt/
RUN rm -rf /converter

ENV FOLDER_IN=/books
ENV FOLDER_OUT=/audiobooks

CMD /opt/audiobookconverter
