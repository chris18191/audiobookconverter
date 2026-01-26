FROM python:3.12-slim-bookworm

RUN apt update
RUN apt install -y ffmpeg espeak-ng
RUN pip install audiblez 
RUN apt install golang

COPY . /converter
WORKDIR /converter
RUN go build .
COPY ./audiobookconverter /opt/
WORKDIR /opt/
RUN rm -rf /converter

ENV FOLDER_IN=/books
ENV FOLDER_OUT=/audiobooks

CMD /opt/audiobookconverter
