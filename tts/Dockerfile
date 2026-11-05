# Speech synthesis for the station announcements (Piper, offline, Russian voice "ruslan").
FROM python:3.11-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg curl \
 && rm -rf /var/lib/apt/lists/*

RUN pip install --no-cache-dir piper-tts

# The voice is part of the image: the service works without internet access.
ARG VOICE_URL=https://huggingface.co/rhasspy/piper-voices/resolve/main/ru/ru_RU/ruslan/medium/ru_RU-ruslan-medium.onnx
RUN mkdir -p /voices \
 && curl -fsSL -o /voices/ru_RU-ruslan-medium.onnx "$VOICE_URL" \
 && curl -fsSL -o /voices/ru_RU-ruslan-medium.onnx.json "$VOICE_URL.json"

WORKDIR /app
COPY server.py /app/server.py
EXPOSE 5002
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD curl -fs http://localhost:5002/healthz | grep -q OK
CMD ["python3", "/app/server.py"]
