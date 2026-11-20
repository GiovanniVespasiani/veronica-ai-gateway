#!/usr/bin/env bash
# Generate a minimal CycloneDX SBOM for a locally built image.
#
# Syft/Trivy are not available in this environment, so this script emits a
# hand-built inventory: the application, the Go runtime used to build it, and
# the base image digests. It deliberately does not claim to enumerate OS
# packages.
set -euo pipefail

IMAGE="${1:?usage: scripts/sbom.sh <image>}"
OUT="${2:-sbom/$(echo "$IMAGE" | tr '/:' '__').cdx.json}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

APP_VERSION="${APP_VERSION:-0.1.1}"
GO_IMAGE="$(awk '/^FROM .* AS build/{print $2}' "$ROOT/Dockerfile" | head -1)"
RUN_IMAGE="$(awk '/^FROM /{img=$2} END{print img}' "$ROOT/Dockerfile")"

img_field() { docker image inspect "$1" --format "$2" 2>/dev/null || echo "unknown"; }

APP_ID="$(img_field "$IMAGE" '{{.Id}}')"
APP_CREATED="$(img_field "$IMAGE" '{{.Created}}')"
GO_DIGEST="$(img_field "$GO_IMAGE" '{{index .RepoDigests 0}}')"
RUN_DIGEST="$(img_field "$RUN_IMAGE" '{{index .RepoDigests 0}}')"
GO_VERSION="$(docker run --rm --entrypoint go "$GO_IMAGE" version 2>/dev/null | awk '{print $3}')"

mkdir -p "$(dirname "$OUT")"
python3 - "$OUT" "$IMAGE" "$APP_VERSION" "$APP_ID" "$APP_CREATED" "$GO_IMAGE" "$GO_DIGEST" "$GO_VERSION" "$RUN_IMAGE" "$RUN_DIGEST" <<'PY'
import json, sys, datetime
(out, image, version, image_id, created, go_img, go_digest, go_ver, run_img, run_digest) = sys.argv[1:11]
def comp(name, ver, typ, purl=None, extra=None):
    c = {"type": typ, "name": name, "version": ver}
    if purl: c["purl"] = purl
    if extra: c["properties"] = [{"name": k, "value": v} for k, v in extra.items()]
    return c
doc = {
  "bomFormat": "CycloneDX",
  "specVersion": "1.5",
  "serialNumber": "urn:uuid:" + __import__("uuid").uuid4().hex,
  "version": 1,
  "metadata": {
    "timestamp": datetime.datetime.now(datetime.timezone.utc).isoformat(),
    "tools": [{"vendor": "veronica", "name": "sbom.sh", "version": "1"}],
    "component": comp("veronica-ai-gateway", version, "application",
                      f"pkg:golang/github.com/veronica/ai-gateway@{version}",
                      {"image.id": image_id, "image.created": created,
                       "note": "hand-generated; OS packages not enumerated"})
  },
  "components": [
    comp(go_img.split(":")[0], go_ver, "container", None, {"image.digest": go_digest, "role": "build-stage"}),
    comp(run_img.split("/")[-1].split(":")[0], run_img.split(":")[-1], "container", None,
         {"image.digest": run_digest, "role": "runtime-stage"}),
  ],
  "dependencies": [{"ref": f"pkg:golang/github.com/veronica/ai-gateway@{version}", "dependsOn": []}]
}
json.dump(doc, open(out, "w"), indent=2)
print(out)
PY

echo "wrote $OUT"
