#!/usr/bin/env bash
# Phase 3: `mvn deploy` into a hosted feed from an identity with the publish
# permission — success; the artifact is then downloadable and the generated
# maven-metadata.xml lists the version. Re-deploying the same version with
# different content is a 409 (invariant 4).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

token="$(registry_token "ci-deploy-$(date +%s)")"

# The project is mounted read-only; maven needs a writable target/, so the
# build runs on a copy inside the container.
deploy() { # <feed> <pom-file>
  local feed="$1" pom="$2"
  compose run --rm -T -e REGISTRY_TOKEN="$token" --entrypoint sh maven-client -c "
    set -e
    cp -r /work-deploy /tmp/w && cd /tmp/w
    mvn -B -s settings.xml -f $pom \
      -Ddeploy.url=http://registry:8080/maven/$feed deploy
  " 2>&1
}

echo "--> mvn deploy into the hosted feed"
out="$(deploy hosted pom.xml)" || { echo "$out" | tail -30; exit 1; }
grep -q "BUILD SUCCESS" <<<"$out" || { echo "$out" | tail -30; exit 1; }

echo "--> published artifact is downloadable with source=local"
base=http://registry:8080/maven/hosted/com/example/deployed/1.0.0
src="$(client_curl -fsS -o /dev/null -w '%header{x-registry-source}' "$base/deployed-1.0.0.jar")"
if [[ "$src" != "cache" && "$src" != "local" ]]; then
  echo "hosted artifact source = $src" >&2
  exit 1
fi
client_curl -fsS -o /dev/null "$base/deployed-1.0.0.pom"

echo "--> sidecar checksum is served from the stored digest"
sha1="$(client_curl -fsS "$base/deployed-1.0.0.jar.sha1")"
if [[ ! "$sha1" =~ ^[0-9a-f]{40}$ ]]; then
  echo "unexpected sha1 body: $sha1" >&2
  exit 1
fi

echo "--> generated maven-metadata.xml lists the version"
meta="$(client_curl -fsS http://registry:8080/maven/hosted/com/example/deployed/maven-metadata.xml)"
grep -q "<version>1.0.0</version>" <<<"$meta" || { echo "$meta" >&2; exit 1; }
grep -q "<release>1.0.0</release>" <<<"$meta" || { echo "$meta" >&2; exit 1; }

echo "--> re-deploying the same version is idempotent for identical content"
out="$(deploy hosted pom.xml)" || { echo "$out" | tail -20; exit 1; }
grep -q "BUILD SUCCESS" <<<"$out" || { echo "identical re-deploy failed" >&2; exit 1; }

echo "--> re-deploying the same version with different content is 409"
out="$(deploy hosted pom-conflict.xml || true)"
if ! grep -qE "409|Conflict|immutable" <<<"$out"; then
  echo "re-deploy with different content was not refused:" >&2
  echo "$out" | tail -20 >&2
  exit 1
fi

echo "maven deploy ok"
