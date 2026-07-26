#!/usr/bin/env bash
# Phase 4: the upstream serves a tarball whose bytes do not match the
# dist.integrity published in its own metadata. The registry must refuse to
# serve it and must not cache it (invariant 5).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

URL=http://registry:8080/npm/npmjs/tampered/-/tampered-1.0.0.tgz

code="$(client_curl -sS -o /dev/null -w '%{http_code}' "$URL")"
if [[ "$code" != "502" ]]; then
  echo "tampered tarball returned $code, want 502" >&2
  exit 1
fi

echo "--> nothing was cached"
listing="$(compose exec -T minio sh -c \
  "mc alias set local http://127.0.0.1:9000 registry registry-secret >/dev/null && \
   mc ls --recursive local/registry/manifests/npmjs/ || true")"
if grep -q "tampered" <<<"$listing"; then
  echo "manifest stored despite integrity mismatch" >&2
  exit 1
fi

echo "--> npm itself also refuses to install the package"
out="$(compose run --rm -T npm-client sh -c '
  cd /tmp && rm -rf t && mkdir t && cd t
  npm config set registry http://registry:8080/npm/npmjs/
  npm install tampered@1.0.0 --no-audit --no-fund 2>&1 || true
')"
if grep -qE 'added 1 package' <<<"$out"; then
  echo "npm installed a tampered package:" >&2
  echo "$out" | tail -10 >&2
  exit 1
fi

echo "npm tampered tarball rejected ok"
