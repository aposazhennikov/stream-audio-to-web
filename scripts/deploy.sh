#!/usr/bin/env bash
# Deploys audio-streamer on the server. Runs from the app directory.
#
# What it does:
#   1. clones the repo if APP_DIR is missing (never touches a non-git APP_DIR)
#   2. backs up .env, telegram_alerts.json and relay_data/relay_list.json
#   3. updates only the deploy files (docker-compose.yml, .env.template,
#      scripts/deploy.sh) from origin/BRANCH
#   4. creates missing state files (.env, telegram_alerts.json, relay_list.json);
#      existing state files are never overwritten
#   5. docker compose pull + up -d, waits for the container to be healthy
#   6. checks every route from DIRECTORY_ROUTES streams audio/mpeg
#   7. logs the running version and warns if it is not the deployed commit
#
# Environment variables:
#   APP_DIR, REPO_URL, BRANCH, DOCKER     paths and commands (defaults below)
#   STATUS_PASSWORD, SENTRY_DSN           used only when .env is created
#   TELEGRAM_BOT_TOKEN, TELEGRAM_CHAT_ID  used only when telegram_alerts.json is created
#   HEALTH_TIMEOUT_SEC, ROUTES_TIMEOUT_SEC  waiting limits, default 300 each
set -euo pipefail

APP_DIR="${APP_DIR:-/home/stream-audio-to-web}"
REPO_URL="${REPO_URL:-https://github.com/aposazhennikov/stream-audio-to-web.git}"
BRANCH="${BRANCH:-main}"
DOCKER="${DOCKER:-docker}"
HEALTH_TIMEOUT_SEC="${HEALTH_TIMEOUT_SEC:-300}"
ROUTES_TIMEOUT_SEC="${ROUTES_TIMEOUT_SEC:-300}"
STATUS_PASSWORD="${STATUS_PASSWORD:-}"
SENTRY_DSN="${SENTRY_DSN:-}"
TELEGRAM_BOT_TOKEN="${TELEGRAM_BOT_TOKEN:-}"
TELEGRAM_CHAT_ID="${TELEGRAM_CHAT_ID:-}"

CONTAINER_NAME="audio-streamer"
APP_PORT=8000
BACKUP_KEEP=10
DEPLOY_FILES=(docker-compose.yml .env.template scripts/deploy.sh)
STATE_FILES=(.env telegram_alerts.json relay_data/relay_list.json)

# DOCKER may hold several words, for example "sudo docker".
read -r -a DOCKER_CMD <<< "$DOCKER"

log() { printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
die() { printf '[%s] ERROR: %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# Writes a single KEY=VALUE line into a .env-style file. The value goes through
# ENVIRON, so "/", "&", "|" and backslashes are kept as they are.
set_env_value() {  # file key value
  local file="$1" key="$2" value="$3" tmp
  tmp="$(mktemp "$file.tmp.XXXXXX")"
  ENV_KEY="$key" ENV_VAL="$value" awk '
    BEGIN { k = ENVIRON["ENV_KEY"]; v = ENVIRON["ENV_VAL"]; done = 0 }
    index($0, k "=") == 1 { if (!done) print k "=" v; done = 1; next }
    { print }
    END { if (!done) print k "=" v }
  ' "$file" > "$tmp"
  cat "$tmp" > "$file"
  rm -f "$tmp"
}

json_escape() {
  local s="$1"
  s="${s//\\/\\\\}"
  s="${s//\"/\\\"}"
  printf '%s' "$s"
}

ensure_repo() {
  if [ -d "$APP_DIR/.git" ]; then
    return 0
  fi
  if [ -e "$APP_DIR" ]; then
    die "$APP_DIR exists but has no .git; not touching it"
  fi
  log "cloning $REPO_URL into $APP_DIR"
  git clone --quiet "$REPO_URL" "$APP_DIR"
}

backup_state() {
  local stamp dir f kept=0
  stamp="$(date -u +%Y%m%dT%H%M%SZ)"
  dir="$APP_DIR/backups/$stamp"
  for f in "${STATE_FILES[@]}"; do
    if [ -f "$APP_DIR/$f" ]; then
      mkdir -p "$dir/$(dirname "$f")"
      cp -p "$APP_DIR/$f" "$dir/$f"
      kept=1
    fi
  done
  if [ "$kept" -eq 1 ]; then
    log "state backed up to $dir"
  fi
  prune_backups
}

prune_backups() {
  local root="$APP_DIR/backups" old
  [ -d "$root" ] || return 0
  # Backup names are UTC timestamps, so reverse lexical order is newest first.
  # Only directories with a timestamp name are candidates for removal.
  find "$root" -mindepth 1 -maxdepth 1 -type d -name '[0-9]*T[0-9]*Z' -printf '%f\n' \
    | sort -r \
    | tail -n +"$((BACKUP_KEEP + 1))" \
    | while IFS= read -r old; do
        log "removing old backup $old"
        rm -r -- "${root:?}/${old:?}"
      done
}

update_deploy_files() {
  local p
  for p in "${DEPLOY_FILES[@]}"; do
    if git -C "$APP_DIR" cat-file -e "FETCH_HEAD:$p" 2>/dev/null; then
      git -C "$APP_DIR" checkout --quiet FETCH_HEAD -- "$p"
      log "updated $p"
    else
      log "skipped $p (not present in $BRANCH)"
    fi
  done
}

ensure_env() {
  local env_file="$APP_DIR/.env"
  if [ -f "$env_file" ]; then
    log ".env present, left untouched"
    return 0
  fi
  [ -f "$APP_DIR/.env.template" ] || die ".env.template not found, cannot create .env"
  cp "$APP_DIR/.env.template" "$env_file"
  chmod 600 "$env_file"
  if [ -n "$STATUS_PASSWORD" ]; then
    set_env_value "$env_file" STATUS_PASSWORD "$STATUS_PASSWORD"
  fi
  if [ -n "$SENTRY_DSN" ]; then
    set_env_value "$env_file" SENTRY_DSN "$SENTRY_DSN"
  fi
  log ".env created from .env.template"
}

ensure_telegram_config() {
  local file="$APP_DIR/telegram_alerts.json" ts
  if [ -f "$file" ]; then
    log "telegram_alerts.json present, left untouched"
    return 0
  fi
  ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  if have python3; then
    TG_TOKEN="$TELEGRAM_BOT_TOKEN" TG_CHAT="$TELEGRAM_CHAT_ID" TG_TS="$ts" python3 -c '
import json, os
cfg = {
    "enabled": True,
    "bot_token": os.environ["TG_TOKEN"],
    "chat_id": os.environ["TG_CHAT"],
    "routes": {},
    "relay_routes": {},
    "updated_at": os.environ["TG_TS"],
}
print(json.dumps(cfg, indent=2))
' > "$file"
  elif have jq; then
    jq -n --arg token "$TELEGRAM_BOT_TOKEN" --arg chat "$TELEGRAM_CHAT_ID" --arg ts "$ts" \
      '{enabled: true, bot_token: $token, chat_id: $chat, routes: {}, relay_routes: {}, updated_at: $ts}' > "$file"
  else
    printf '{\n  "enabled": true,\n  "bot_token": "%s",\n  "chat_id": "%s",\n  "routes": {},\n  "relay_routes": {},\n  "updated_at": "%s"\n}\n' \
      "$(json_escape "$TELEGRAM_BOT_TOKEN")" "$(json_escape "$TELEGRAM_CHAT_ID")" "$ts" > "$file"
  fi
  chmod 600 "$file"
  log "telegram_alerts.json created"
}

ensure_relay_list() {
  local file="$APP_DIR/relay_data/relay_list.json"
  mkdir -p "$APP_DIR/relay_data"
  if [ -f "$file" ]; then
    return 0
  fi
  if git -C "$APP_DIR" cat-file -e "FETCH_HEAD:relay_data/relay_list.json" 2>/dev/null; then
    git -C "$APP_DIR" show "FETCH_HEAD:relay_data/relay_list.json" > "$file"
    log "relay_data/relay_list.json restored from git"
  else
    log "relay_data/relay_list.json missing and not in git, left as is"
  fi
}

deploy_containers() {
  cd "$APP_DIR"
  log "pulling image"
  "${DOCKER_CMD[@]}" compose pull
  log "starting containers"
  "${DOCKER_CMD[@]}" compose up -d
}

wait_healthy() {
  local status="" deadline
  deadline=$((SECONDS + HEALTH_TIMEOUT_SEC))
  while :; do
    status="$("${DOCKER_CMD[@]}" inspect --format '{{.State.Health.Status}}' "$CONTAINER_NAME" 2>/dev/null || true)"
    if [ "$status" = "healthy" ]; then
      log "container $CONTAINER_NAME is healthy"
      return 0
    fi
    if [ "$SECONDS" -ge "$deadline" ]; then
      log "container status is '${status:-unknown}' after ${HEALTH_TIMEOUT_SEC}s, last 100 log lines:"
      "${DOCKER_CMD[@]}" logs --tail 100 "$CONTAINER_NAME" 2>&1 || true
      die "container did not become healthy"
    fi
    log "container status: ${status:-unknown}, waiting"
    sleep 5
  done
}

# Prints the route names from DIRECTORY_ROUTES in .env, one per line.
list_routes() {
  local env_file="$APP_DIR/.env" raw
  raw="$(awk -F= '$1 == "DIRECTORY_ROUTES" { sub(/^[^=]*=/, ""); print; exit }' "$env_file" | tr -d '\r')"
  [ -n "$raw" ] || die "DIRECTORY_ROUTES not found in .env"
  if have jq; then
    printf '%s' "$raw" | jq -r 'keys[] | ltrimstr("/")' || die "DIRECTORY_ROUTES is not valid JSON"
  elif have python3; then
    printf '%s' "$raw" | python3 -c '
import json, sys
try:
    routes = json.load(sys.stdin)
except ValueError:
    sys.exit(1)
for name in routes:
    print(name.lstrip("/"))
' || die "DIRECTORY_ROUTES is not valid JSON"
  else
    die "neither jq nor python3 is installed"
  fi
}

# Prints "<http_code> <content_type> <size_download>" for one route.
check_route() {
  curl -s -m 4 -o /dev/null -w '%{http_code} %{content_type} %{size_download}' "http://localhost:$APP_PORT/$1" || true
}

verify_routes() {
  local routes_raw route out code ctype size state failed=0 deadline
  local -a routes pending still
  local -A result=()

  routes_raw="$(list_routes)"
  [ -n "$routes_raw" ] || die "DIRECTORY_ROUTES has no routes"
  mapfile -t routes <<< "$routes_raw"
  pending=("${routes[@]}")

  deadline=$((SECONDS + ROUTES_TIMEOUT_SEC))
  while :; do
    still=()
    for route in "${pending[@]}"; do
      out="$(check_route "$route")"
      code=""; ctype=""; size=""
      read -r code ctype size <<< "$out" || true
      if [ "$code" = "200" ] && [ "${ctype%%;*}" = "audio/mpeg" ] && [ "${size:-0}" -gt 0 ] 2>/dev/null; then
        result[$route]="OK|$code|$ctype|$size"
      else
        result[$route]="FAIL|${code:-000}|${ctype:-}|${size:-0}"
        still+=("$route")
      fi
    done
    pending=("${still[@]}")
    if [ "${#pending[@]}" -eq 0 ]; then
      break
    fi
    if [ "$SECONDS" -ge "$deadline" ]; then
      break
    fi
    log "${#pending[@]} route(s) not ready yet, retrying in 10s"
    sleep 10
  done

  printf '\n%-22s %-5s %-14s %-10s %s\n' ROUTE HTTP TYPE SIZE RESULT
  for route in "${routes[@]}"; do
    IFS='|' read -r state code ctype size <<< "${result[$route]}"
    printf '%-22s %-5s %-14s %-10s %s\n' "$route" "$code" "${ctype:--}" "$size" "$state"
    if [ "$state" != "OK" ]; then
      failed=$((failed + 1))
    fi
  done
  printf '\n'

  if [ "$failed" -gt 0 ]; then
    die "$failed of ${#routes[@]} route(s) failed"
  fi
  log "all ${#routes[@]} routes stream audio/mpeg"
}

# Logs the version of the running container and warns if it was not built from the
# deployed commit (for example, the image push lagged behind). Never fails the deploy.
report_version() {
  local body expected commit
  body="$(curl -s -m 4 "http://localhost:$APP_PORT/version" || true)"
  if [ -z "$body" ]; then
    log "version: not reported by the running container"
    return 0
  fi
  log "running version: $body"
  expected="$(git -C "$APP_DIR" rev-parse FETCH_HEAD 2>/dev/null || true)"
  commit="$(printf '%s' "$body" | sed -n 's/.*"commit":"\([0-9a-f]*\)".*/\1/p')"
  if [ -n "$expected" ] && [ -n "$commit" ] && [ "$commit" != "$expected" ]; then
    log "WARNING: running commit $commit differs from deployed $BRANCH $expected"
  fi
}

main() {
  ensure_repo
  log "fetching $BRANCH from origin"
  git -C "$APP_DIR" fetch --quiet origin "$BRANCH"

  backup_state
  update_deploy_files
  ensure_env
  ensure_telegram_config
  ensure_relay_list

  deploy_containers
  wait_healthy
  cd "$APP_DIR"
  verify_routes
  report_version
  log "deploy finished"
}

main "$@"
