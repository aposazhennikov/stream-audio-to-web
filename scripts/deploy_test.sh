#!/usr/bin/env bash
# Local test for scripts/deploy.sh. Needs no docker, no server and no network.
#
# A throwaway bare repo is built from the current working tree, so the test
# works with uncommitted changes. docker and curl are replaced by stubs on PATH
# or through the DOCKER variable. Each scenario runs deploy.sh against its own
# temporary APP_DIR. Exit status is 0 only if every check passes.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEPLOY="$ROOT/scripts/deploy.sh"
TMP="$(mktemp -d)"

PASS=0
FAILS=0
RUN_NO=0
RC=0
OUT=""

SECRET_PASSWORD='p/a&ss|w'
SECRET_DSN='https://key@o1.ingest.de.sentry.io/12&3'
SECRET_TOKEN='123456:AAF/x&y"z'
SECRET_CHAT='-1001234567890'

check() {  # description, command...
  local desc="$1"
  shift
  if "$@" > /dev/null 2>&1; then
    PASS=$((PASS + 1))
    echo "PASS $desc"
  else
    FAILS=$((FAILS + 1))
    echo "FAIL $desc"
  fi
}

has_line() { grep -qxF -- "$2" "$1"; }                 # file, exact line
logged() { grep -qxF -- "$1" "$DOCKER_LOG"; }          # exact docker call
not_logged() { ! logged "$1"; }

json_field_is() {  # file, key, expected string
  python3 - "$1" "$2" "$3" <<'PYEOF'
import json, sys
path, key, expected = sys.argv[1:4]
with open(path, encoding="utf-8") as fh:
    data = json.load(fh)
sys.exit(0 if data.get(key) == expected else 1)
PYEOF
}

show_output_on_fail() {
  if [ "$RC" -ne 0 ]; then
    echo "----- deploy output -----"
    cat "$OUT"
    echo "-------------------------"
  fi
}

# Runs deploy.sh for APP_DIR. Sets RC and OUT.
run_deploy() {
  local app="$1"
  RUN_NO=$((RUN_NO + 1))
  OUT="$TMP/out-$RUN_NO.log"
  APP_DIR="$app" \
  REPO_URL="$REPO" \
  BRANCH=main \
  DOCKER="$STUB/docker" \
  DOCKER_LOG="$DOCKER_LOG" \
  HEALTH_TIMEOUT_SEC=5 \
  ROUTES_TIMEOUT_SEC=0 \
  STATUS_PASSWORD="$SECRET_PASSWORD" \
  SENTRY_DSN="$SECRET_DSN" \
  TELEGRAM_BOT_TOKEN="$SECRET_TOKEN" \
  TELEGRAM_CHAT_ID="$SECRET_CHAT" \
  PATH="$STUB:$PATH" \
  bash "$DEPLOY" > "$OUT" 2>&1
  RC=$?
}

# ---------------------------------------------------------------- fixtures

SRC="$TMP/src"
REPO="$TMP/remote.git"
STUB="$TMP/bin"
DOCKER_LOG="$TMP/docker.log"

mkdir -p "$SRC/scripts" "$SRC/relay_data" "$STUB"
cp "$ROOT/docker-compose.yml" "$ROOT/.env.template" "$SRC/"
cp "$DEPLOY" "$SRC/scripts/deploy.sh"
cp "$ROOT/relay_data/relay_list.json" "$SRC/relay_data/relay_list.json"
git -C "$SRC" init --quiet
git -C "$SRC" symbolic-ref HEAD refs/heads/main
git -C "$SRC" add -A
git -C "$SRC" -c user.name=deploy-test -c user.email=deploy-test@localhost commit --quiet -m "snapshot"
git clone --quiet --bare "$SRC" "$REPO"

cat > "$STUB/docker" <<'EOF'
#!/bin/bash
echo "$*" >> "$DOCKER_LOG"
case "$1" in
  inspect) echo healthy ;;
  logs) echo "stub log line" ;;
esac
exit 0
EOF

cat > "$STUB/curl" <<'EOF'
#!/bin/bash
url="${*: -1}"
if [ "$url" = "http://localhost:8000/version" ]; then
  printf '{"version":"test","commit":"%s"}\n' "${FAKE_COMMIT:-0000000}"
elif [ -n "${FAKE_404_ROUTE:-}" ] && [ "$url" = "http://localhost:8000/$FAKE_404_ROUTE" ]; then
  printf '404 text/html 15'
else
  printf '200 audio/mpeg 1000'
fi
exit 0
EOF
chmod +x "$STUB/docker" "$STUB/curl"

# ---------------------------------------------------------- (a) first run

echo "== (a) first run on an empty APP_DIR"
APP_A="$TMP/app-a"
: > "$DOCKER_LOG"
run_deploy "$APP_A"
show_output_on_fail
check "(a) deploy exits 0" test "$RC" -eq 0
check "(a) .env created" test -f "$APP_A/.env"
check "(a) .env has STATUS_PASSWORD with / & |" has_line "$APP_A/.env" "STATUS_PASSWORD=$SECRET_PASSWORD"
check "(a) .env has SENTRY_DSN with &" has_line "$APP_A/.env" "SENTRY_DSN=$SECRET_DSN"
check "(a) .env keeps other template keys" has_line "$APP_A/.env" "LOG_LEVEL=info"
check "(a) running version is logged" grep -q 'running version: {"version":"test"' "$OUT"
check "(a) commit mismatch is reported" grep -q 'WARNING: running commit 0000000 differs' "$OUT"
check "(a) telegram_alerts.json is valid JSON" python3 -m json.tool "$APP_A/telegram_alerts.json"
check "(a) telegram bot_token survives escaping" json_field_is "$APP_A/telegram_alerts.json" bot_token "$SECRET_TOKEN"
check "(a) telegram chat_id written" json_field_is "$APP_A/telegram_alerts.json" chat_id "$SECRET_CHAT"
check "(a) relay_data/relay_list.json exists" cmp -s "$APP_A/relay_data/relay_list.json" "$ROOT/relay_data/relay_list.json"
check "(a) docker compose pull called" logged "compose pull"
check "(a) docker compose up -d called" logged "compose up -d"
check "(a) docker compose down never called" not_logged "compose down"

# ------------------------------------------- (b) second run keeps the state

echo "== (b) second run after hand edits"
printf '# hand edit\nMANUAL_KEY=keep/me&now\n' >> "$APP_A/.env"
printf '{"enabled": false, "bot_token": "hand", "chat_id": "1", "routes": {"humor": {"chat": "x"}}, "relay_routes": {}, "updated_at": "2020-01-01T00:00:00Z"}\n' > "$APP_A/telegram_alerts.json"
printf '{"relays": [{"name": "hand-edited"}]}\n' > "$APP_A/relay_data/relay_list.json"

mkdir -p "$TMP/saved"
cp "$APP_A/.env" "$TMP/saved/env"
cp "$APP_A/telegram_alerts.json" "$TMP/saved/telegram_alerts.json"
cp "$APP_A/relay_data/relay_list.json" "$TMP/saved/relay_list.json"

# Twelve old backups, so retention (keep 10) has something to remove.
for i in $(seq 0 11); do
  mkdir -p "$APP_A/backups/$(printf '20200101T0000%02dZ' "$i")"
done

: > "$DOCKER_LOG"
run_deploy "$APP_A"
show_output_on_fail
check "(b) deploy exits 0" test "$RC" -eq 0
check "(b) .env byte-identical" cmp -s "$APP_A/.env" "$TMP/saved/env"
check "(b) telegram_alerts.json byte-identical" cmp -s "$APP_A/telegram_alerts.json" "$TMP/saved/telegram_alerts.json"
check "(b) relay_list.json byte-identical" cmp -s "$APP_A/relay_data/relay_list.json" "$TMP/saved/relay_list.json"

NEWEST="$APP_A/backups/$(find "$APP_A/backups" -mindepth 1 -maxdepth 1 -printf '%f\n' | sort | tail -n 1)"
check "(b) newest backup holds .env copy" cmp -s "$NEWEST/.env" "$TMP/saved/env"
check "(b) newest backup holds telegram_alerts.json copy" cmp -s "$NEWEST/telegram_alerts.json" "$TMP/saved/telegram_alerts.json"
check "(b) only 10 backups kept" test "$(find "$APP_A/backups" -mindepth 1 -maxdepth 1 | wc -l)" -eq 10
check "(b) oldest backup removed" test ! -e "$APP_A/backups/20200101T000000Z"
check "(b) docker compose pull called" logged "compose pull"
check "(b) docker compose up -d called" logged "compose up -d"
check "(b) docker compose down never called" not_logged "compose down"

# ---------------------------------------- (c) APP_DIR without .git is refused

echo "== (c) APP_DIR exists without .git"
APP_C="$TMP/app-c"
mkdir -p "$APP_C"
printf 'precious\n' > "$APP_C/.env"
cp "$APP_C/.env" "$TMP/precious"
: > "$DOCKER_LOG"
run_deploy "$APP_C"
check "(c) deploy exits 1" test "$RC" -eq 1
check "(c) existing .env untouched" cmp -s "$APP_C/.env" "$TMP/precious"
check "(c) no other files created in APP_DIR" test "$(find "$APP_C" -mindepth 1 -maxdepth 1 | wc -l)" -eq 1
check "(c) docker not called" test ! -s "$DOCKER_LOG"

# ------------------------------------------------ (d) failing route fails

echo "== (d) one route returns 404"
APP_D="$TMP/app-d"
: > "$DOCKER_LOG"
export FAKE_404_ROUTE=humor
run_deploy "$APP_D"
unset FAKE_404_ROUTE
check "(d) deploy exits 1" test "$RC" -eq 1
check "(d) humor row marked FAIL" grep -qE '^humor +404 .*FAIL$' "$OUT"
check "(d) nature row marked OK" grep -qE '^nature +200 .*OK$' "$OUT"

# -------------------------------- (e) missing state files are recreated

echo "== (e) .env, telegram_alerts.json and relay_list.json missing"
APP_E="$TMP/app-e"
cp -a "$APP_A" "$APP_E"
rm -f "$APP_E/.env" "$APP_E/telegram_alerts.json" "$APP_E/relay_data/relay_list.json"
: > "$DOCKER_LOG"
run_deploy "$APP_E"
show_output_on_fail
check "(e) deploy exits 0" test "$RC" -eq 0
check "(e) .env recreated with secret" has_line "$APP_E/.env" "STATUS_PASSWORD=$SECRET_PASSWORD"
check "(e) telegram_alerts.json recreated as valid JSON" python3 -m json.tool "$APP_E/telegram_alerts.json"
check "(e) telegram bot_token recreated" json_field_is "$APP_E/telegram_alerts.json" bot_token "$SECRET_TOKEN"
check "(e) relay_list.json restored from git" cmp -s "$APP_E/relay_data/relay_list.json" "$ROOT/relay_data/relay_list.json"

# ---------------------------------------------------------------- summary

echo
echo "PASS: $PASS  FAIL: $FAILS"
if [ "$FAILS" -gt 0 ]; then
  echo "temporary files kept in $TMP"
  exit 1
fi
rm -rf -- "$TMP"
exit 0
