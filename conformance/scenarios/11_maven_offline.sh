#!/usr/bin/env bash
# Repeat dependency:resolve with the fake upstream STOPPED: a fresh client
# container (no local repo) must succeed entirely from the registry cache.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

restore_upstream() {
  compose up -d --wait fake-upstream >/dev/null 2>&1 || true
}
trap restore_upstream EXIT

echo "--> stopping fake-upstream"
compose stop fake-upstream >/dev/null

echo "--> mvn dependency:resolve again (fresh container, upstream down)"
out="$(compose run --rm -T maven-client -B -s /work/settings.xml -f /work/pom.xml org.apache.maven.plugins:maven-dependency-plugin:3.6.1:resolve 2>&1)" || {
  echo "$out" | tail -40
  exit 1
}
grep -q "BUILD SUCCESS" <<<"$out" || { echo "offline resolve failed" >&2; exit 1; }

src="$(client_curl -fsS -o /dev/null -w '%header{x-registry-source}' \
  http://registry:8080/maven/central/com/example/liba/1.0.0/liba-1.0.0.jar)"
if [[ "$src" != "cache" ]]; then
  echo "artifact source = $src, want cache" >&2
  exit 1
fi

echo "maven offline resolve ok"
