#!/usr/bin/env bash
# Development stack: mock upstream + gateway (testmode) on loopback.
# Used by the isolated n8n compatibility test. Never used in production.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WORK:-$(mktemp -d "${TMPDIR:-/tmp}/veronica-devstack.XXXXXX")}"
mkdir -p "$WORK"

GW_PORT="${GW_PORT:-18080}"
MOCK_PORT="${MOCK_PORT:-19100}"
CLIENT_TOKEN="${CLIENT_TOKEN:-local-client-token-0123456789}"
UPSTREAM_TOKEN="${UPSTREAM_TOKEN:-mock-upstream-token-0123456789}"

go build -C "$ROOT" -o "$WORK/gateway" -tags testmode ./cmd/gateway || exit 1
go build -C "$ROOT" -o "$WORK/mock" ./cmd/mockupstream || exit 1

"$WORK/mock" -addr "127.0.0.1:${MOCK_PORT}" -token "$UPSTREAM_TOKEN" >"$WORK/mock.log" 2>&1 &
MOCK_PID=$!

GATEWAY_LISTEN_ADDR="127.0.0.1:${GW_PORT}" \
GATEWAY_CLIENT_TOKEN="$CLIENT_TOKEN" \
GATEWAY_UPSTREAM_TOKEN="$UPSTREAM_TOKEN" \
GATEWAY_TEST_UPSTREAM_URL="http://127.0.0.1:${MOCK_PORT}/openai/v1" \
GATEWAY_TEST_SESSION_MODE="${SESSION_MODE:-mock}" \
GATEWAY_MAX_BODY_BYTES=4194304 \
GATEWAY_MAX_INFLIGHT=8 \
GATEWAY_RATE_PER_MINUTE=600 \
GATEWAY_RATE_BURST=60 \
"$WORK/gateway" >"$WORK/gateway.log" 2>&1 &
GW_PID=$!

cleanup() { kill "$GW_PID" "$MOCK_PID" 2>/dev/null; wait 2>/dev/null; }
trap cleanup EXIT

echo "work=$WORK gateway=$GW_PID mock=$MOCK_PID"
wait
