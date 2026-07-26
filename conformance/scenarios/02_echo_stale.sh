#!/usr/bin/env bash
# Mutable metadata survives an upstream outage: after the TTL expires and
# the upstream is down, the registry serves the stale copy with
# X-Registry-Source: stale (invariant 6).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

URL=http://registry:8080/echo/test/meta/info.json
TTL_SECONDS=5 # keep in sync with echomodule.MetadataTTL

restore_upstream() {
  compose up -d --wait fake-upstream >/dev/null 2>&1 || true
}
trap restore_upstream EXIT

src="$(client_curl -fsS -o /dev/null -w '%header{x-registry-source}' "$URL")"
if [[ "$src" != "upstream" ]]; then
  echo "first fetch source = $src, want upstream" >&2
  exit 1
fi
original="$(client_curl -fsS "$URL")"

echo "--> stopping fake-upstream and waiting out the ${TTL_SECONDS}s TTL"
compose stop fake-upstream >/dev/null
sleep $((TTL_SECONDS + 1))

src="$(client_curl -fsS -o /dev/null -w '%header{x-registry-source}' "$URL")"
if [[ "$src" != "stale" ]]; then
  echo "post-TTL fetch source = $src, want stale" >&2
  exit 1
fi
body="$(client_curl -fsS "$URL")"
if [[ "$body" != "$original" ]]; then
  echo "stale body mismatch: $body != $original" >&2
  exit 1
fi

echo "echo stale ok"
