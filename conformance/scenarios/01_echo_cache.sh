#!/usr/bin/env bash
# Phase 1 acceptance: an artifact fetched through the registry from the fake
# upstream is served from cache (X-Registry-Source: cache) after the
# upstream is stopped.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

URL=http://registry:8080/echo/test/data/hello.txt
EXPECTED="hello from the fake upstream"

restore_upstream() {
  compose up -d --wait fake-upstream >/dev/null 2>&1 || true
}
trap restore_upstream EXIT

src="$(client_curl -fsS -o /dev/null -w '%header{x-registry-source}' "$URL")"
if [[ "$src" != "upstream" ]]; then
  echo "first fetch source = $src, want upstream" >&2
  exit 1
fi

body="$(client_curl -fsS "$URL")"
if [[ "$body" != "$EXPECTED" ]]; then
  echo "body mismatch: $body" >&2
  exit 1
fi

src="$(client_curl -fsS -o /dev/null -w '%header{x-registry-source}' "$URL")"
if [[ "$src" != "cache" ]]; then
  echo "second fetch source = $src, want cache" >&2
  exit 1
fi

echo "--> stopping fake-upstream"
compose stop fake-upstream >/dev/null

src="$(client_curl -fsS -o /dev/null -w '%header{x-registry-source}' "$URL")"
body="$(client_curl -fsS "$URL")"
if [[ "$src" != "cache" || "$body" != "$EXPECTED" ]]; then
  echo "offline fetch: source=$src body=$body" >&2
  exit 1
fi

echo "echo cache ok"
