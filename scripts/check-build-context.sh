#!/usr/bin/env bash
# Fails if the Docker build context appears to contain credentials.
# Runs in CI before any image is built and can be run locally.
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.." || exit 1

PATTERNS='oc_sk_[A-Za-z0-9]|sk-ant-|-----BEGIN (RSA |EC |OPENSSH )?PRIVATE KEY-----|ghp_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}'
FOUND=0

# Only files that would be sent to the builder.
while IFS= read -r f; do
  case "$f" in
    ./.git/*|./sbom/*|./docs/*|./scripts/*|./*.md) continue ;;
  esac
  if grep -IqE "$PATTERNS" "$f" 2>/dev/null; then
    echo "credential-like pattern in build context: $f" >&2
    FOUND=1
  fi
done < <(find . -type f -not -path './.git/*' -not -path './sbom/*')

if [ "$FOUND" -ne 0 ]; then
  echo "refusing to build: remove the credential material above" >&2
  exit 1
fi

# .dockerignore must exclude the secret example and local artefacts.
for required in 'secret.example.yaml' '.build/' 'bin/'; do
  grep -qF "$required" .dockerignore 2>/dev/null || {
    echo ".dockerignore missing entry: $required" >&2
    exit 1
  }
done

echo "build context clean"
