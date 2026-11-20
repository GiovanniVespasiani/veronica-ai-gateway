#!/usr/bin/env bash
# Real-process memory measurement.
#
# Runs the gateway against the loopback mock and drives concurrent large bodies
# while sampling the process RSS. macOS: ps -o rss= reports KiB.
#
# Usage: tests/memory.sh [body_mib] [concurrency]
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/veronica-mem.XXXXXX")"
BIN="$WORK/bin"
mkdir -p "$BIN"

BODY_MIB="${1:-16}"
CONC="${2:-8}"
GW_PORT=18081
MOCK_PORT=19101
GW="http://127.0.0.1:${GW_PORT}"
MOCK="http://127.0.0.1:${MOCK_PORT}"
CLIENT_TOKEN="local-client-token-0123456789"
UPSTREAM_TOKEN="mock-upstream-token-0123456789"

GW_PID=""
MOCK_PID=""
SAMPLER_PID=""

cleanup() {
  [ -n "$SAMPLER_PID" ] && kill "$SAMPLER_PID" 2>/dev/null
  [ -n "$GW_PID" ] && kill "$GW_PID" 2>/dev/null
  [ -n "$MOCK_PID" ] && kill "$MOCK_PID" 2>/dev/null
  wait 2>/dev/null
}
trap cleanup EXIT

( cd "$ROOT" && go build -o "$BIN/gateway" -tags testmode ./cmd/gateway ) || exit 1
( cd "$ROOT" && go build -o "$BIN/mock" ./cmd/mockupstream ) || exit 1

python3 - "$WORK/body.json" "$BODY_MIB" <<'PY'
import json, sys
path, mib = sys.argv[1], int(sys.argv[2])
pad = "x" * (mib * 1024 * 1024)
json.dump({"model": "deepseek-v4.1-flash",
           "messages": [{"role": "user", "content": pad}]}, open(path, "w"))
PY
ACTUAL=$(wc -c < "$WORK/body.json")

"$BIN/mock" -addr "127.0.0.1:${MOCK_PORT}" -token "$UPSTREAM_TOKEN" >"$WORK/mock.log" 2>&1 &
MOCK_PID=$!
for _ in $(seq 1 50); do
  curl -fsS -o /dev/null -H "Authorization: Bearer $UPSTREAM_TOKEN" "$MOCK/v1/models" 2>/dev/null && break
  sleep 0.1
done

GATEWAY_LISTEN_ADDR="127.0.0.1:${GW_PORT}" \
GATEWAY_CLIENT_TOKEN="$CLIENT_TOKEN" \
GATEWAY_UPSTREAM_TOKEN="$UPSTREAM_TOKEN" \
GATEWAY_TEST_UPSTREAM_URL="${MOCK}/openai/v1" \
GATEWAY_TEST_SESSION_MODE="mock" \
GATEWAY_MAX_BODY_BYTES=$((BODY_MIB*1024*1024 + 1024)) \
GATEWAY_MAX_INFLIGHT="$CONC" \
GATEWAY_RATE_PER_MINUTE=6000 \
GATEWAY_RATE_BURST=500 \
"$BIN/gateway" >"$WORK/gateway.log" 2>&1 &
GW_PID=$!
for _ in $(seq 1 50); do
  curl -fsS -o /dev/null "$GW/healthz" 2>/dev/null && break
  sleep 0.1
done
curl -fsS -o /dev/null "$GW/healthz" || { echo "gateway failed to start"; cat "$WORK/gateway.log"; exit 1; }

BASELINE_KB=$(ps -o rss= -p "$GW_PID" | tr -d ' ')

: >"$WORK/rss.txt"
(
  while kill -0 "$GW_PID" 2>/dev/null; do
    ps -o rss= -p "$GW_PID" | tr -d ' '
    sleep 0.05
  done
) >"$WORK/rss.txt" &
SAMPLER_PID=$!

START=$(date +%s)
seq 1 "$CONC" | xargs -P "$CONC" -I{} curl -s -o /dev/null -w '%{http_code}\n' \
  -X POST "$GW/v1/chat/completions" \
  -H "Authorization: Bearer $CLIENT_TOKEN" \
  -H "X-Gateway-Test-Conversation: mem-{}" \
  --data-binary "@$WORK/body.json" >"$WORK/codes.txt"
END=$(date +%s)

sleep 0.2
kill "$SAMPLER_PID" 2>/dev/null
SAMPLER_PID=""

PEAK_KB=$(sort -n "$WORK/rss.txt" | tail -1)
CODES=$(sort "$WORK/codes.txt" | uniq -c | tr '\n' ' ')

python3 - "$BASELINE_KB" "$PEAK_KB" "$ACTUAL" "$CONC" "$CODES" "$((END-START))" <<'PY'
import sys
base, peak, size, conc, codes, secs = sys.argv[1:7]
base_kb, peak_kb, size_b = int(base), int(peak), int(size)
print(f"body_bytes          {size_b/1048576:.2f} MiB")
print(f"concurrency         {conc}")
print(f"responses           {codes.strip()}")
print(f"wall_seconds        {secs}")
print(f"rss_baseline        {base_kb/1024:.1f} MiB")
print(f"rss_peak            {peak_kb/1024:.1f} MiB")
print(f"rss_delta           {(peak_kb-base_kb)/1024:.1f} MiB")
print(f"peak/body ratio     {peak_kb*1024/size_b:.2f}x per request")
PY
