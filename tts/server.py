"""Speech synthesis service for the station announcements.

POST /speak?rate=48000 with the text as the request body (UTF-8) returns the spoken text as
MP3 at that sample rate (44.1 kHz without ?rate), stereo, 128 kbps (64 kbps below 32 kHz), no
ID3 tag and no Xing frame, so the audio server can insert it into a running MP3 stream of the
same rate: the receiver goes silent when the rate changes inside a stream. GET /healthz
answers "OK".

The voice is loaded once at start; synthesis is serialised (one CPU is enough for short
phrases).
"""
import io
import os
import subprocess
import threading
import wave
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

from piper import PiperVoice

MODEL = os.environ.get("TTS_MODEL", "/voices/ru_RU-ruslan-medium.onnx")
PORT = int(os.environ.get("TTS_PORT", "5002"))
MAX_TEXT = 1000  # characters

voice = PiperVoice.load(MODEL)
lock = threading.Lock()


RATES = {8000, 11025, 12000, 16000, 22050, 24000, 32000, 44100, 48000}


def synthesize_mp3(text: str, rate: int = 44100) -> bytes:
    bitrate = "128k" if rate >= 32000 else "64k" if rate >= 16000 else "32k"
    buf = io.BytesIO()
    with lock:
        with wave.open(buf, "wb") as wav:
            if hasattr(voice, "synthesize_wav"):
                voice.synthesize_wav(text, wav)
            else:
                voice.synthesize(text, wav)
    # A short silence after the phrase separates it from what follows in the stream.
    result = subprocess.run(
        ["ffmpeg", "-nostdin", "-v", "error", "-f", "wav", "-i", "pipe:0",
         "-af", "aresample=%d,apad=pad_dur=0.4" % rate, "-ac", "2",
         "-c:a", "libmp3lame", "-b:a", bitrate, "-id3v2_version", "0", "-write_xing", "0",
         "-f", "mp3", "pipe:1"],
        input=buf.getvalue(), capture_output=True, check=True)
    return result.stdout


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/healthz":
            self._reply(200, b"OK", "text/plain")
        else:
            self._reply(404, b"not found", "text/plain")

    def do_POST(self):
        url = urlparse(self.path)
        if url.path != "/speak":
            self._reply(404, b"not found", "text/plain")
            return
        try:
            rate = int(parse_qs(url.query).get("rate", ["44100"])[0])
        except ValueError:
            rate = 0
        if rate not in RATES:
            self._reply(400, b"unsupported rate", "text/plain")
            return
        length = int(self.headers.get("Content-Length") or 0)
        text = self.rfile.read(min(length, MAX_TEXT * 4)).decode("utf-8", "replace").strip()[:MAX_TEXT]
        if not text:
            self._reply(400, b"empty text", "text/plain")
            return
        try:
            audio = synthesize_mp3(text, rate)
        except Exception as err:  # noqa: BLE001
            self._reply(500, str(err).encode(), "text/plain")
            return
        self._reply(200, audio, "audio/mpeg")

    def _reply(self, code, body, content_type):
        self.send_response(code)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt, *args):  # quiet: the audio server logs what matters
        pass


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
