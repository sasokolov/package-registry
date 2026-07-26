#!/usr/bin/env bash
# LIVE: resolve a real dependency from Maven Central through the registry.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

echo "--> mvn dependency:resolve (org.slf4j:slf4j-api) via registry -> Maven Central"
out="$(compose run --rm -T maven-client -B -s /work/settings.xml -f /work-live/pom.xml \
  org.apache.maven.plugins:maven-dependency-plugin:3.6.1:resolve 2>&1)" || {
  echo "$out" | tail -40
  exit 1
}
grep -q "BUILD SUCCESS" <<<"$out" || { echo "live resolve failed" >&2; exit 1; }
grep -q "org.slf4j:slf4j-api:jar:2.0.13" <<<"$out" || { echo "slf4j-api not resolved" >&2; exit 1; }

echo "--> second fetch is a cache hit (checksum verified against Central's .sha1 at ingest)"
src="$(client_curl -fsS -o /dev/null -w '%header{x-registry-source}' \
  http://registry:8080/maven/central/org/slf4j/slf4j-api/2.0.13/slf4j-api-2.0.13.jar)"
if [[ "$src" != "cache" ]]; then
  echo "source = $src, want cache" >&2
  exit 1
fi

echo "live maven ok"
