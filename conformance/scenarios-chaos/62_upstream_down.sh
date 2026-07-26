#!/usr/bin/env bash
# Chaos: stop the upstream. Metadata is served stale (invariant 6) and a
# real build still completes from cache.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

restore_upstream() {
  compose up -d --wait fake-upstream >/dev/null 2>&1 || true
}
trap restore_upstream EXIT

META=http://lb/maven/central/com/example/liba/maven-metadata.xml

echo "--> warming the cache"
client_curl -fsS -o /dev/null "$META"
compose run --rm -T npm-client sh -c '
  set -e
  cp -r /src /tmp/w && cd /tmp/w
  npm config set registry http://lb/npm/npmjs/
  npm ci --no-audit --no-fund >/dev/null
' >/dev/null

echo "--> stopping the upstream and waiting out the metadata TTL"
compose stop fake-upstream >/dev/null
sleep 6

echo "--> metadata is served stale"
src="$(client_curl -fsS -o /dev/null -w '%header{x-registry-source}' "$META")"
if [[ "$src" != "stale" && "$src" != "cache" ]]; then
  echo "metadata source = $src, want stale (or cache within TTL)" >&2
  exit 1
fi
client_curl -fsS "$META" | grep -q "<artifactId>liba</artifactId>" || {
  echo "stale metadata is not the cached document" >&2; exit 1; }

echo "--> a build still completes entirely from cache"
out="$(compose run --rm -T npm-client sh -c '
  set -e
  cp -r /src /tmp/w2 && cd /tmp/w2
  npm config set registry http://lb/npm/npmjs/
  npm ci --no-audit --no-fund
  echo BUILD_OK
' 2>&1)" || { echo "$out" | tail -20; exit 1; }
grep -q BUILD_OK <<<"$out" || { echo "$out" | tail -20; exit 1; }

echo "upstream outage survived"
