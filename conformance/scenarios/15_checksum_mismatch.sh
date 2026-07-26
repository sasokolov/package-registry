#!/usr/bin/env bash
# Invariant 5 end-to-end: the upstream publishes a .sha1 that does not match
# the jar (tampered mirror). The registry must refuse to serve it and must
# not cache anything — no manifest, no blob.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

URL=http://registry:8080/maven/central/com/example/libc/1.0.0/libc-1.0.0.jar

code="$(client_curl -sS -o /dev/null -w '%{http_code}' "$URL")"
if [[ "$code" != "502" ]]; then
  echo "tampered artifact returned $code, want 502" >&2
  exit 1
fi

echo "--> nothing was cached for the rejected artifact"
listing="$(compose exec -T minio sh -c \
  "mc alias set local http://127.0.0.1:9000 registry registry-secret >/dev/null && \
   mc ls --recursive local/registry/manifests/central/ || true")"
if grep -q "libc-1.0.0.jar" <<<"$listing"; then
  echo "manifest stored despite checksum mismatch" >&2
  exit 1
fi

real_sha256="$(sha256sum "$CONFORMANCE_DIR/fixtures/maven/com/example/libc/1.0.0/libc-1.0.0.jar" | cut -d' ' -f1)"
blobs="$(compose exec -T minio sh -c \
  "mc alias set local http://127.0.0.1:9000 registry registry-secret >/dev/null && \
   mc ls --recursive local/registry/blobs/sha256/ || true")"
if grep -q "$real_sha256" <<<"$blobs"; then
  echo "blob stored despite checksum mismatch" >&2
  exit 1
fi

echo "--> a second request still fails (no negative caching turning into success)"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' "$URL")"
if [[ "$code" != "502" ]]; then
  echo "second attempt returned $code, want 502" >&2
  exit 1
fi

echo "checksum mismatch rejection ok"
