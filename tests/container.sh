#!/usr/bin/env bash
# Container hardening check for the built image.
# Runs the image under the same constraints the Kubernetes manifest requests:
# non-root, read-only rootfs, all capabilities dropped, no new privileges.
set -uo pipefail
IMAGE="${1:-veronica-ai-gateway:0.1.1}"
PORT="${PORT:-18083}"
CONTAINER="veronica-gw-hardening"
CLIENT_TOKEN="container-client-token-0123456789"

cleanup() { docker rm -f "$CONTAINER" >/dev/null 2>&1; }
trap cleanup EXIT
cleanup

echo "== configured user =="
docker image inspect "$IMAGE" --format '{{.Config.User}}'

echo "== start hardened =="
docker run -d --name "$CONTAINER" -p "127.0.0.1:${PORT}:8080" \
  --read-only --user 65532:65532 --cap-drop ALL \
  --security-opt no-new-privileges \
  --memory 256m --pids-limit 64 \
  -e GATEWAY_LISTEN_ADDR=:8080 \
  -e GATEWAY_CLIENT_TOKEN="$CLIENT_TOKEN" \
  -e GATEWAY_UPSTREAM_TOKEN="container-upstream-token-0123456789" \
  "$IMAGE" >/dev/null

for _ in $(seq 1 40); do
  curl -fsS -o /dev/null "http://127.0.0.1:${PORT}/healthz" 2>/dev/null && break
  sleep 0.25
done
printf 'healthz: '; curl -sS -o /dev/null -w '%{http_code}\n' "http://127.0.0.1:${PORT}/healthz"
printf 'readyz:  '; curl -sS -o /dev/null -w '%{http_code}\n' "http://127.0.0.1:${PORT}/readyz"

echo "== fail-closed production behaviour =="
curl -sS -o /tmp/container-501.json -w 'status=%{http_code}\n' \
  -X POST "http://127.0.0.1:${PORT}/v1/chat/completions" \
  -H "Authorization: Bearer $CLIENT_TOKEN" \
  -d '{"model":"deepseek-v4.1-flash","messages":[]}'
head -c 200 /tmp/container-501.json; echo

echo "== no shell in image =="
if docker run --rm --entrypoint /bin/sh "$IMAGE" -c true >/dev/null 2>&1; then
  echo "FAIL: a shell is present"
else
  echo "ok: no shell"
fi

echo "== process uid =="
docker exec "$CONTAINER" /gateway --version >/dev/null 2>&1
docker inspect "$CONTAINER" --format 'user={{.Config.User}}'
