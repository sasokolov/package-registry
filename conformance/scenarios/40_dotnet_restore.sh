#!/usr/bin/env bash
# Phase 5: `dotnet restore` through the registry (service index synthesized
# locally, flat container and gzipped registration proxied) — success; then
# the same restore with the fake upstream stopped — success from cache.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

restore_upstream() {
  compose up -d --wait fake-upstream >/dev/null 2>&1 || true
}
trap restore_upstream EXIT

dotnet_restore() {
  compose run --rm -T dotnet-client sh -c '
    set -e
    rm -rf /tmp/w && cp -r /src /tmp/w && cd /tmp/w
    dotnet restore --no-cache
    test -d packages/conformance.lib/1.2.3
  ' 2>&1
}

echo "--> service index is served by the registry itself"
index="$(client_curl -fsS http://registry:8080/nuget/nugetorg/v3/index.json)"
grep -q '"PackageBaseAddress/3.0.0"' <<<"$index" || { echo "$index" >&2; exit 1; }
grep -q 'registry:8080/nuget/nugetorg/v3/flat2/' <<<"$index" || {
  echo "service index does not point at the registry:" >&2; echo "$index" >&2; exit 1; }
src="$(client_curl -fsS -o /dev/null -w '%header{x-registry-source}' \
  http://registry:8080/nuget/nugetorg/v3/index.json)"
if [[ "$src" != "local" ]]; then
  echo "service index source = $src, want local" >&2
  exit 1
fi

echo "--> gzipped registration document is served as plain JSON with registry URLs"
reg="$(client_curl -fsS http://registry:8080/nuget/nugetorg/v3/registration/conformance.lib/index.json)"
grep -q '"packageContent"' <<<"$reg" || { echo "$reg" | head -c 400 >&2; exit 1; }
# Every endpoint the client will follow (package content, registration
# pages) must point at the registry. Catalog URLs are not served here and
# stay upstream on purpose.
if grep -oE '"packageContent":"[^"]*"' <<<"$reg" | grep -q 'fake-upstream'; then
  echo "packageContent still points at the upstream" >&2
  exit 1
fi
grep -q 'registry:8080/nuget/nugetorg/v3/flat2/' <<<"$reg" || {
  echo "packageContent not repointed at the registry:" >&2; echo "$reg" | head -c 400 >&2; exit 1; }
grep -q 'registry:8080/nuget/nugetorg/v3/registration/' <<<"$reg" || {
  echo "registration pages not repointed:" >&2; echo "$reg" | head -c 400 >&2; exit 1; }

echo "--> dotnet restore"
out="$(dotnet_restore)" || { echo "$out" | tail -30; exit 1; }

echo "--> stopping fake-upstream and restoring again"
compose stop fake-upstream >/dev/null
out="$(dotnet_restore)" || { echo "$out" | tail -30; exit 1; }

src="$(client_curl -fsS -o /dev/null -w '%header{x-registry-source}' \
  "http://registry:8080/nuget/nugetorg/v3/flat2/conformance.lib/1.2.3/conformance.lib.1.2.3.nupkg")"
if [[ "$src" != "cache" ]]; then
  echo "nupkg source = $src, want cache" >&2
  exit 1
fi

echo "dotnet restore ok"
