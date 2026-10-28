#!/usr/bin/env bash
# Long local check before a release: builds the image, generates real MP3 files,
# starts the container and runs soak_test.py against it, then decodes what the
# listeners received to make sure it is real, non-silent audio.
#
# Usage: scripts/soak/run_soak.sh            (40 minutes, image from the working tree)
#        MINUTES=5 REF=HEAD scripts/soak/run_soak.sh   (image from a git ref)
# Env: MINUTES, PORT, IMAGE, NAME, WORK, REF, KEEP=1 (leave the container running).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MINUTES="${MINUTES:-40}"
PORT="${PORT:-18000}"
IMAGE="${IMAGE:-audio-streamer:soak}"
NAME="${NAME:-audio-streamer-soak}"
WORK="${WORK:-$HOME/.cache/audio-streamer-soak}"
REF="${REF:-}"
PASSWORD="soak-$(date +%s)-$RANDOM"
AUDIO="$WORK/audio"
OUT="$WORK/out"

log() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }

log "building image $IMAGE"
if [ -n "$REF" ]; then
  git -C "$ROOT" archive "$REF" | docker build -q -t "$IMAGE" - >/dev/null
else
  docker build -q -t "$IMAGE" "$ROOT" >/dev/null
fi

log "generating test audio in $AUDIO"
mkdir -p "$AUDIO/short" "$AUDIO/long" "$AUDIO/ru"
rm -rf "$OUT"
mkdir -p "$OUT"
docker run --rm --entrypoint sh -v "$AUDIO:/w" "$IMAGE" -c '
set -e
enc() { [ -f "$1" ] || ffmpeg -nostdin -loglevel error -f lavfi -i "sine=frequency=$2:duration=$3" -ac 2 -ar 44100 -c:a libmp3lame $4 -metadata title="$5" "$1"; }
i=1
while [ $i -le 30 ]; do
  enc "$(printf /w/short/%02d.mp3 $i)" $((200 + i * 25)) $((15 + i % 20)) "-b:a 128k" "Short $i"
  i=$((i + 1))
done
enc /w/long/long1.mp3 220 720 "-b:a 128k" "Long 1"
enc /w/long/long2.mp3 330 720 "-b:a 128k" "Long 2"
enc "/w/ru/01 Песня о друге.mp3" 440 60 "-b:a 128k" "Песня о друге"
enc "/w/ru/02 Д'"'"'Артаньян и три мушкетёра.mp3" 523 60 "-b:a 128k" "Д'"'"'Артаньян"
enc "/w/ru/03 Вальс VBR.mp3" 659 60 "-q:a 4" "Вальс"
'

log "starting container $NAME on port $PORT"
docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" -p "$PORT:8000" --memory 256m --cpus 1 \
  -e DIRECTORY_ROUTES='{"short":"/app/short","long":"/app/long","ru":"/app/ru"}' \
  -e STATUS_PASSWORD="$PASSWORD" -e LOG_LEVEL=info -e BITRATE=128 -e MAX_CLIENTS=30 \
  -e TG_ALERT=false -e RELAY=false -e SENTRY_DSN= \
  -v "$AUDIO/short:/app/short" -v "$AUDIO/long:/app/long" -v "$AUDIO/ru:/app/ru" \
  "$IMAGE" >/dev/null

cleanup() {
  if [ "${KEEP:-0}" != "1" ]; then
    docker rm -f "$NAME" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

log "waiting for all routes"
for route in short long ru; do
  for _ in $(seq 1 90); do
    code="$(curl -s -m 3 -o /dev/null -w '%{http_code}' "http://localhost:$PORT/$route" || true)"
    [ "$code" = "200" ] && break
    sleep 2
  done
  [ "$code" = "200" ] || { log "route $route not ready"; docker logs --tail 50 "$NAME"; exit 1; }
done

log "running soak test for $MINUTES minutes"
set +e
python3 -I "$ROOT/scripts/soak/soak_test.py" --base-url "http://localhost:$PORT" \
  --minutes "$MINUTES" --password "$PASSWORD" --out-dir "$OUT" --stall-seconds "${STALL_SECONDS:-600}" \
  --skip "${SKIP:-}"
soak_rc=$?
set -e

log "decoding received audio"
decode_rc=0
for f in "$OUT"/*.mp3; do
  [ -s "$f" ] || { log "EMPTY capture: $f"; decode_rc=1; continue; }
  # volumedetect prints at info level; decode problems are counted from the error-level run.
  report="$(docker run --rm --entrypoint ffmpeg -v "$OUT:/o" "$IMAGE" -nostdin -hide_banner -i "/o/$(basename "$f")" -af volumedetect -f null - 2>&1 || true)"
  mean="$(printf '%s\n' "$report" | sed -n 's/.*mean_volume: \(-*[0-9.]*\) dB.*/\1/p' | tail -1)"
  errors="$(docker run --rm --entrypoint ffmpeg -v "$OUT:/o" "$IMAGE" -nostdin -v error -i "/o/$(basename "$f")" -f null - 2>&1 | grep -c . || true)"
  printf '  %-12s %8s bytes  mean_volume=%s dB  decode_errors=%s\n' "$(basename "$f")" "$(stat -c %s "$f")" "${mean:-?}" "$errors"
  if [ -z "$mean" ] || awk -v m="$mean" 'BEGIN { exit !(m < -40) }'; then
    log "SILENT or undecodable: $f"
    decode_rc=1
  fi
  if [ "$errors" -gt 5 ]; then
    log "too many decode errors: $f"
    decode_rc=1
  fi
done

log "container status: $(docker inspect -f '{{.State.Status}} restarts={{.RestartCount}} oom={{.State.OOMKilled}}' "$NAME")"
log "errors in app log: $(docker logs "$NAME" 2>&1 | grep -c '"level":"ERROR"' || true)"
docker logs "$NAME" 2>&1 | grep '"level":"ERROR"' | cut -c1-200 | sort | uniq -c | sort -rn | head -10 || true

if [ "$soak_rc" -eq 0 ] && [ "$decode_rc" -eq 0 ]; then
  log "SOAK PASSED"
  exit 0
fi
log "SOAK FAILED (soak=$soak_rc decode=$decode_rc)"
exit 1
