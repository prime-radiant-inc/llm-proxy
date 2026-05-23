#!/usr/bin/env bash
# Smoke test for PRI-1800: verify gzipped Anthropic responses are logged as
# readable JSON (not corrupted gzip bytes).
#
# Usage:
#   ./tests/smoke_gzip_decode.sh
#
# Requires:
#   - ANTHROPIC_API_KEY in env, or in ~/.amplifier/keys.env
#   - curl, jq, go
#
# Exit codes:
#   0 - response body is readable JSON containing usage.input_tokens
#   1 - any failure (key missing, build fail, body corrupted, etc.)

set -euo pipefail

cd "$(dirname "$0")/.."

if [[ -z "${ANTHROPIC_API_KEY:-}" && -f "$HOME/.amplifier/keys.env" ]]; then
  # shellcheck disable=SC1091
  source "$HOME/.amplifier/keys.env"
fi

if [[ -z "${ANTHROPIC_API_KEY:-}" ]]; then
  echo "ERROR: ANTHROPIC_API_KEY not set and not found in ~/.amplifier/keys.env" >&2
  exit 1
fi

LOG_DIR="$(mktemp -d)"
trap 'rm -rf "$LOG_DIR"; if [[ -n "${PROXY_PID:-}" ]]; then kill "$PROXY_PID" 2>/dev/null || true; fi' EXIT

echo "[smoke] building llm-proxy..."
go build -o /tmp/llm-proxy-smoke .

PORT=18071
echo "[smoke] starting proxy on port $PORT, log dir $LOG_DIR..."
/tmp/llm-proxy-smoke --port "$PORT" --log-dir "$LOG_DIR" >/tmp/llm-proxy-smoke.log 2>&1 &
PROXY_PID=$!

# Wait for proxy to bind
for _ in $(seq 1 30); do
  if curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 || \
     nc -z 127.0.0.1 "$PORT" 2>/dev/null; then
    break
  fi
  sleep 0.1
done

echo "[smoke] sending a real Anthropic call through the proxy (gzip requested)..."
RESPONSE=$(curl -sS \
  -H "x-api-key: $ANTHROPIC_API_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -H "content-type: application/json" \
  -H "accept-encoding: gzip" \
  --compressed \
  "http://127.0.0.1:$PORT/anthropic/api.anthropic.com/v1/messages" \
  -d '{
    "model": "claude-haiku-4-5-20251001",
    "max_tokens": 16,
    "stream": false,
    "messages": [{"role":"user","content":"reply with just the word ok"}]
  }')

echo "[smoke] live API response:"
echo "$RESPONSE" | jq -c .

# Give the proxy a moment to flush
sleep 1

# Find the latest JSONL file
LOG_FILE=$(find "$LOG_DIR/api.anthropic.com" -name '*.jsonl' -type f | sort | tail -1)
if [[ -z "$LOG_FILE" ]]; then
  echo "[smoke] FAIL: no JSONL log file produced" >&2
  cat /tmp/llm-proxy-smoke.log >&2
  exit 1
fi

echo "[smoke] log file: $LOG_FILE"

# Pull the response entry and assert it has a parseable JSON body with usage.input_tokens
LOGGED_BODY=$(jq -rc 'select(.type=="response") | .body' "$LOG_FILE" | tail -1)
if [[ -z "$LOGGED_BODY" ]]; then
  echo "[smoke] FAIL: no response entry in log" >&2
  cat "$LOG_FILE" >&2
  exit 1
fi

echo "[smoke] logged response body (first 200 chars):"
echo "${LOGGED_BODY:0:200}"

# Must parse as JSON and have usage.input_tokens
INPUT_TOKENS=$(echo "$LOGGED_BODY" | jq -e '.usage.input_tokens' 2>/dev/null || true)
if [[ -z "$INPUT_TOKENS" || "$INPUT_TOKENS" == "null" ]]; then
  echo "[smoke] FAIL: logged response body is not readable JSON or has no usage.input_tokens" >&2
  echo "[smoke] hex prefix of body:" >&2
  echo -n "${LOGGED_BODY:0:32}" | xxd | head -2 >&2
  exit 1
fi

echo "[smoke] PASS: logged response body is readable JSON with usage.input_tokens=$INPUT_TOKENS"
