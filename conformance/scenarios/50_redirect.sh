#!/usr/bin/env bash
# Phase 6: redirect mode — a cached tarball is answered with a 302 to a
# pre-signed storage URL, metadata is still streamed, and `npm ci` works
# end to end through the redirect.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

TARBALL=http://registry:8080/npm/npm-redirect/left-pad/-/left-pad-1.3.0.tgz
ROOT=http://registry:8080/npm/npm-redirect/left-pad

echo "--> first fetch ingests the tarball"
client_curl -fsSL -o /dev/null "$TARBALL"

echo "--> the cached tarball is answered with a 302 to a pre-signed URL"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' "$TARBALL")"
if [[ "$code" != "302" ]]; then
  echo "redirect-mode tarball returned $code, want 302" >&2
  exit 1
fi
location="$(client_curl -sS -o /dev/null -w '%header{location}' "$TARBALL")"
if [[ "$location" != *"X-Amz-Signature"* ]]; then
  echo "Location is not a pre-signed URL: $location" >&2
  exit 1
fi
src="$(client_curl -sS -o /dev/null -w '%header{x-registry-source}' "$TARBALL")"
if [[ "$src" != "cache" ]]; then
  echo "redirect response source = $src, want cache" >&2
  exit 1
fi

echo "--> following the redirect yields the tarball"
size="$(client_curl -fsSL -o /dev/null -w '%{size_download}' "$TARBALL")"
if (( size < 100 )); then
  echo "redirected download returned $size bytes" >&2
  exit 1
fi

echo "--> metadata is still streamed, never redirected"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' "$ROOT")"
if [[ "$code" != "200" ]]; then
  echo "package root returned $code, want 200 (metadata must not redirect)" >&2
  exit 1
fi

echo "--> npm ci works through the redirect feed"
out="$(compose run --rm -T npm-client sh -c '
  set -e
  cp -r /src /tmp/wr && cd /tmp/wr
  npm config set registry http://registry:8080/npm/npm-redirect/
  rm -f package-lock.json
  npm install --no-audit --no-fund
  node -e "require(\"left-pad\"); require(\"is-odd\"); require(\"@scope/util\"); console.log(\"redirect install ok\")"
' 2>&1)" || { echo "$out" | tail -20; exit 1; }
grep -q "redirect install ok" <<<"$out" || { echo "$out" | tail -20; exit 1; }

echo "redirect mode ok"
