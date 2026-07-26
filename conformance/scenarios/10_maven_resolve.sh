#!/usr/bin/env bash
# mvn dependency:resolve of the reference project through the registry
# (settings.xml mirror). All artifacts must land in MinIO as content-
# addressed blobs, and sidecar checksums must be served from stored digests.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

echo "--> mvn dependency:resolve via registry mirror"
out="$(compose run --rm -T maven-client -B -s /work/settings.xml -f /work/pom.xml org.apache.maven.plugins:maven-dependency-plugin:3.6.1:resolve 2>&1)" || {
  echo "$out" | tail -40
  exit 1
}
grep -q "BUILD SUCCESS" <<<"$out" || { echo "no BUILD SUCCESS in mvn output" >&2; exit 1; }
grep -q "com.example:liba:jar:1.0.0" <<<"$out" || { echo "liba not resolved" >&2; exit 1; }
grep -q "com.example:libb:jar:1.0.0" <<<"$out" || { echo "libb not resolved" >&2; exit 1; }

echo "--> artifacts stored in MinIO (manifests + content-addressed blobs)"
listing="$(compose exec -T minio sh -c \
  "mc alias set local http://127.0.0.1:9000 registry registry-secret >/dev/null && \
   mc ls --recursive local/registry/manifests/central/ && mc ls --recursive local/registry/blobs/sha256/ | head -20")"
for want in liba-1.0.0.jar liba-1.0.0.pom libb-1.0.0.jar libb-1.0.0.pom; do
  grep -q "$want" <<<"$listing" || { echo "manifest for $want missing in MinIO" >&2; exit 1; }
done

echo "--> sidecar checksum served from stored digest matches the fixture"
fixture_sha1="$(cat "$CONFORMANCE_DIR/fixtures/maven/com/example/liba/1.0.0/liba-1.0.0.jar.sha1")"
served_sha1="$(client_curl -fsS http://registry:8080/maven/central/com/example/liba/1.0.0/liba-1.0.0.jar.sha1)"
if [[ "$served_sha1" != "$fixture_sha1" ]]; then
  echo "served sha1 $served_sha1 != fixture $fixture_sha1" >&2
  exit 1
fi
# sha256 sidecar is served even though the upstream never published one.
client_curl -fsS -o /dev/null http://registry:8080/maven/central/com/example/liba/1.0.0/liba-1.0.0.jar.sha256

echo "maven resolve ok"
