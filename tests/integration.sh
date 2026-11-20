#!/usr/bin/env bash
# End-to-end local integration test.
#
# Builds the gateway and the mock upstream, runs them on loopback, and exercises
# the documented behaviour with curl. No network access, no provider call, no
# Kubernetes.
#
# Usage: tests/integration.sh
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/veronica-gw.XXXXXX")"
BIN="$WORK/bin"
mkdir -p "$BIN"

GW_PORT="${GW_PORT:-18080}"
MOCK_PORT="${MOCK_PORT:-19099}"
GW="http://127.0.0.1:${GW_PORT}"
MOCK="http://127.0.0.1:${MOCK_PORT}"

CLIENT_TOKEN="local-client-token-0123456789"
UPSTREAM_TOKEN="mock-upstream-token-0123456789"
PROMPT_SENTINEL="SENTINEL_PROMPT_local_only"

PASS=0
FAIL=0
GW_PID=""
MOCK_PID=""

cleanup() {
  [ -n "$GW_PID" ] && kill "$GW_PID" 2>/dev/null
  [ -n "$MOCK_PID" ] && kill "$MOCK_PID" 2>/dev/null
  wait 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT

ok()   { PASS=$((PASS+1)); printf '  ok   %s\n' "$1"; }
bad()  { FAIL=$((FAIL+1)); printf '  FAIL %s\n' "$1"; }
check() { # name, condition-exit-code
  if [ "$2" -eq 0 ]; then ok "$1"; else bad "$1"; fi
}

echo "== build"
( cd "$ROOT" && go build -o "$BIN/gateway-test" -tags testmode ./cmd/gateway ) || exit 1
( cd "$ROOT" && go build -o "$BIN/mockupstream" ./cmd/mockupstream ) || exit 1
ok "binaries built"

echo "== start mock upstream"
"$BIN/mockupstream" -addr "127.0.0.1:${MOCK_PORT}" -token "$UPSTREAM_TOKEN" >"$WORK/mock.log" 2>&1 &
MOCK_PID=$!

for _ in $(seq 1 50); do
  if curl -fsS -o /dev/null -H "Authorization: Bearer $UPSTREAM_TOKEN" "$MOCK/v1/models" 2>/dev/null; then break; fi
  sleep 0.1
done
curl -fsS -o /dev/null -H "Authorization: Bearer $UPSTREAM_TOKEN" "$MOCK/v1/models" \
  || { echo "mock upstream did not start"; cat "$WORK/mock.log"; exit 1; }
ok "mock upstream healthy"

echo "== start gateway (testmode, loopback upstream)"
GATEWAY_LISTEN_ADDR="127.0.0.1:${GW_PORT}" \
GATEWAY_CLIENT_TOKEN="$CLIENT_TOKEN" \
GATEWAY_UPSTREAM_TOKEN="$UPSTREAM_TOKEN" \
GATEWAY_TEST_UPSTREAM_URL="${MOCK}/openai/v1" \
GATEWAY_TEST_SESSION_MODE="mock" \
GATEWAY_RATE_PER_MINUTE=6000 \
GATEWAY_RATE_BURST=500 \
GATEWAY_MAX_INFLIGHT=16 \
"$BIN/gateway-test" >"$WORK/gateway.log" 2>&1 &
GW_PID=$!

for _ in $(seq 1 50); do
  if curl -fsS -o /dev/null "$GW/healthz" 2>/dev/null; then break; fi
  sleep 0.1
done
curl -fsS -o /dev/null "$GW/healthz" || { echo "gateway did not start"; cat "$WORK/gateway.log"; exit 1; }
ok "gateway healthy"

echo "== routing"
code=$(curl -s -o /dev/null -w '%{http_code}' "$GW/v1/admin")
check "unknown route is 404" "$([ "$code" = "404" ] && echo 0 || echo 1)"

code=$(curl -s -o /dev/null -w '%{http_code}' "$GW/healthz")
check "healthz is 200" "$([ "$code" = "200" ] && echo 0 || echo 1)"

body=$(curl -s -X POST "$GW/v1/responses" -H "Authorization: Bearer $CLIENT_TOKEN" -d '{"model":"deepseek-v4.1-flash","input":"hi"}')
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$GW/v1/responses" -H "Authorization: Bearer $CLIENT_TOKEN" -d '{}')
echo "$body" | grep -q '"code":"unknown_url"'
check "responses route reports unknown_url" $?
check "responses route is 404" "$([ "$code" = "404" ] && echo 0 || echo 1)"
echo "$body" | grep -q 'Unknown request URL'
check "responses message matches n8n predicate" $?

echo "== authentication"
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$GW/v1/chat/completions" -d '{}')
check "missing token is 401" "$([ "$code" = "401" ] && echo 0 || echo 1)"
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$GW/v1/chat/completions" -H "Authorization: Bearer wrong" -d '{}')
check "wrong token is 401" "$([ "$code" = "401" ] && echo 0 || echo 1)"

echo "== model allowlist"
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$GW/v1/chat/completions" \
  -H "Authorization: Bearer $CLIENT_TOKEN" -d '{"model":"expensive","messages":[]}')
check "disallowed model is 400" "$([ "$code" = "400" ] && echo 0 || echo 1)"

echo "== chat completion (non-streaming)"
resp=$(curl -s -X POST "$GW/v1/chat/completions" \
  -H "Authorization: Bearer $CLIENT_TOKEN" \
  -H 'Content-Type: application/json' \
  -H "X-Gateway-Test-Conversation: manual-conv-1" \
  -d "{\"model\":\"deepseek-v4.1-flash\",\"messages\":[{\"role\":\"user\",\"content\":\"$PROMPT_SENTINEL\"}]}")
echo "$resp" | grep -q '"content":"mock reply"'
if [ $? -eq 0 ]; then ok "chat completion returns upstream content"; else bad "chat completion returns upstream content (got: $resp)"; fi

state=$(curl -s "$MOCK/__admin/state")
echo "$state" | grep -q '"last_session":"ses_mock_'
check "upstream received a session identifier" $?
echo "$state" | grep -q '"auth_present":true'
check "upstream received gateway bearer" $?

echo "== streaming"
stream=$(curl -s -N -X POST "$GW/v1/chat/completions" \
  -H "Authorization: Bearer $CLIENT_TOKEN" \
  -H 'Content-Type: application/json' \
  -H "X-Gateway-Test-Conversation: manual-conv-2" \
  -d '{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}')
echo "$stream" | grep -q 'data:'
check "stream emits SSE frames" $?
echo "$stream" | grep -q '\[DONE\]'
check "stream terminates with [DONE]" $?

echo "== log hygiene"
if grep -qi "$PROMPT_SENTINEL" "$WORK/gateway.log"; then bad "prompt text found in logs"; else ok "prompt text absent from logs"; fi
if grep -q "$CLIENT_TOKEN" "$WORK/gateway.log"; then bad "client token found in logs"; else ok "client token absent from logs"; fi
if grep -q "$UPSTREAM_TOKEN" "$WORK/gateway.log"; then bad "upstream token found in logs"; else ok "upstream token absent from logs"; fi
if grep -q 'request_id' "$WORK/gateway.log"; then ok "access records carry request_id"; else bad "no access records"; fi

echo "== concurrency"
seq 1 12 | xargs -P 12 -I{} curl -s -o /dev/null -w '%{http_code}\n' -X POST "$GW/v1/chat/completions" \
  -H "Authorization: Bearer $CLIENT_TOKEN" \
  -H "X-Gateway-Test-Conversation: conc-{}" \
  -d '{"model":"deepseek-v4.1-flash","messages":[]}' >"$WORK/conc.txt"
n200=$(grep -c '^200$' "$WORK/conc.txt")
check "12 parallel requests returned 200" "$([ "$n200" = "12" ] && echo 0 || echo 1)"

echo
echo "== summary: ${PASS} passed, ${FAIL} failed"
if [ "$FAIL" -ne 0 ]; then
  echo "gateway log:"
  tail -20 "$WORK/gateway.log"
  exit 1
fi
