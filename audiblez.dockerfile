FROM python:3.12-slim-bookworm

RUN apt update
RUN apt install -y ffmpeg espeak-ng
RUN pip install audiblez 

CMD bash
