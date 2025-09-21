#!/usr/bin/env bash
set -euo pipefail

# Bulk enqueue domains into search_crawler_service via its HTTP API.
# Reads a file with one domain per line (or "rank,domain") and posts root URL.
#
# Env variables (override defaults):
#   API=http://localhost:8082
#   ENDPOINT=$API/api/enqueue
#   FILE=tmp/top-1000000-domains/top-1000000-domains
#   SCHEME=https
#   CONCURRENCY=24
#   PRIORITY=
#   LIMIT=0
#   START_AT=0
#   TIMEOUT=5
#   LOG_EVERY=5000
#   RETRIES=5
#   RETRY_DELAY_MS=200
#   RETRY_JITTER_MS=200
#   SLEEP_MS=0
#   FAILED_FILE=scripts/enqueue_failed.txt
#   CLEAR_FAILED=1
#
# Usage:
#   FILE=tmp/top-1000000-domains/top-1000000-domains ./scripts/enqueue_domains.sh
#   API=http://localhost:8082 CONCURRENCY=24 PRIORITY=5 ./scripts/enqueue_domains.sh

API="${API:-http://localhost:18082}"
ENDPOINT="${ENDPOINT:-$API/api/enqueue}"
FILE="${FILE:-tmp/top-1000000-domains/top-1000000-domains}"
SCHEME="${SCHEME:-https}"
CONCURRENCY="${CONCURRENCY:-24}"
PRIORITY="${PRIORITY:-}"
LIMIT="${LIMIT:-0}"
START_AT="${START_AT:-0}"
TIMEOUT="${TIMEOUT:-5}"
LOG_EVERY="${LOG_EVERY:-5000}"
RETRIES="${RETRIES:-5}"
RETRY_DELAY_MS="${RETRY_DELAY_MS:-200}"
RETRY_JITTER_MS="${RETRY_JITTER_MS:-200}"
SLEEP_MS="${SLEEP_MS:-0}"
FAILED_FILE="${FAILED_FILE:-scripts/enqueue_failed.txt}"
CLEAR_FAILED="${CLEAR_FAILED:-1}"

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  cat <<'EOF'
Usage: [ENV=val ...] ./scripts/enqueue_domains.sh

Environment:
  API, ENDPOINT, FILE, SCHEME, CONCURRENCY, PRIORITY, LIMIT, START_AT, TIMEOUT, LOG_EVERY, RETRIES, RETRY_DELAY_MS, RETRY_JITTER_MS, SLEEP_MS, FAILED_FILE, CLEAR_FAILED
EOF
  exit 0
fi

if ! command -v curl >/dev/null 2>&1; then
  echo "curl is required" >&2
  exit 1
fi

if [[ ! -f "$FILE" ]]; then
  echo "Input file not found: $FILE" >&2
  exit 1
fi

if [[ "$SCHEME" != "http" && "$SCHEME" != "https" ]]; then
  SCHEME="https"
fi

# Init failed file
if [[ "$CLEAR_FAILED" == "1" ]]; then
  : > "$FAILED_FILE"
fi

ms_sleep() {
  local ms="$1"
  if (( ms > 0 )); then
    awk "BEGIN { system(\"sleep \" sprintf(\"%.3f\", $ms/1000)) }" >/dev/null 2>&1
  fi
}

worker() {
  local domain="$1"
  local url="$SCHEME://$domain/"
  local data
  if [[ -n "$PRIORITY" ]]; then
    data=$(printf '{"url":"%s","priority":%s}' "$url" "$PRIORITY")
  else
    data=$(printf '{"url":"%s"}' "$url")
  fi
  local attempt=0
  local max="$RETRIES"
  if [[ "$max" -le 0 ]]; then max=3; fi

  while true; do
    if (( SLEEP_MS > 0 )); then ms_sleep "$SLEEP_MS"; fi
    if curl -sS --http1.1 -H 'Connection: close' -m "$TIMEOUT" -H 'Content-Type: application/json' -X POST -d "$data" "$ENDPOINT" -o /dev/null; then
      return 0
    fi
    attempt=$((attempt+1))
    if (( attempt >= max )); then
      echo "$domain" >> "$FAILED_FILE"
      echo "enqueue failed after $attempt: $domain" >&2
      return 1
    fi
    base=$(( RETRY_DELAY_MS * (2 ** (attempt-1)) ))
    jitter=0
    if (( RETRY_JITTER_MS > 0 )); then
      jitter=$(( RANDOM % (RETRY_JITTER_MS + 1) ))
    fi
    delay=$(( base + jitter ))
    ms_sleep "$delay"
  done
}
export -f worker
export -f ms_sleep
export SCHEME PRIORITY TIMEOUT ENDPOINT RETRIES RETRY_DELAY_MS RETRY_JITTER_MS SLEEP_MS FAILED_FILE

producer() {
  awk '
BEGIN{FS=","}
{
  line=$0
  gsub(/^[ \t]+|[ \t]+$/, "", line)
  if (line=="" || substr(line,1,1)=="#") next
  if (index(line,",")>0) {
    domain=$NF
  } else {
    domain=line
  }
  gsub(/^[ \t".]+|[ \t".]+$/, "", domain)
  if (domain=="") next
  if (domain ~ /^[A-Za-z0-9][A-Za-z0-9\.-]*[A-Za-z0-9]$/) print tolower(domain)
}' "$FILE"
}

start=$(( START_AT + 1 ))
stream_cmd="producer | tail -n +$start"
if [[ "$LIMIT" -gt 0 ]]; then
  stream_cmd="$stream_cmd | head -n $LIMIT"
fi

count=0
progress() {
  while IFS= read -r d; do
    count=$((count+1))
    echo "$d"
    if (( count % LOG_EVERY == 0 )); then
      echo "progress: $count domains" >&2
    fi
  done
}

# shellcheck disable=SC2086
eval "$stream_cmd" | progress | xargs -r -P "$CONCURRENCY" -n 1 -I{} bash -c 'worker "$@"' _ {}

echo "Done. See failed (if any): $FAILED_FILE"