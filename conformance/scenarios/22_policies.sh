#!/usr/bin/env bash
# Policies v1 end-to-end: the license policy refuses a GPL publish, and the
# quarantine policy refuses to serve a too-fresh proxied release.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

token="$(registry_token "ci-policy-$(date +%s)")"

deploy() { # <pom-file>
  compose run --rm -T -e REGISTRY_TOKEN="$token" --entrypoint sh maven-client -c "
    set -e
    cp -r /work-deploy /tmp/w && cd /tmp/w
    mvn -B -s settings.xml -f $1 \
      -Ddeploy.url=http://registry:8080/maven/licensed deploy
  " 2>&1
}

echo "--> deploying an Apache-2.0 artifact into the license-guarded feed"
out="$(deploy pom.xml)" || { echo "$out" | tail -30; exit 1; }
grep -q "BUILD SUCCESS" <<<"$out" || { echo "$out" | tail -30; exit 1; }

echo "--> deploying a GPL-3.0 artifact is refused by the license policy"
out="$(deploy pom-gpl.xml || true)"
if ! grep -qE "status code: 403" <<<"$out"; then
  echo "GPL deploy was not refused:" >&2
  echo "$out" | tail -20 >&2
  exit 1
fi

echo "--> audit log attributes the denial to the license policy"
audit="$(compose logs --no-log-prefix registry | grep '"log":"audit"' | grep '"policy":"license"' || true)"
if [[ -z "$audit" ]]; then
  echo "no audit record attributed to the license policy" >&2
  exit 1
fi

echo "--> quarantine policy blocks a too-fresh proxied release"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' \
  http://registry:8080/maven/fresh/com/example/liba/1.0.0/liba-1.0.0.jar)"
if [[ "$code" != "403" ]]; then
  echo "quarantined artifact returned $code, want 403" >&2
  exit 1
fi
audit="$(compose logs --no-log-prefix registry | grep '"log":"audit"' | grep '"policy":"quarantine"' || true)"
if [[ -z "$audit" ]]; then
  echo "no audit record attributed to the quarantine policy" >&2
  exit 1
fi

echo "--> the same artifact is served from a feed without that policy"
client_curl -fsS -o /dev/null http://registry:8080/maven/central/com/example/liba/1.0.0/liba-1.0.0.jar

echo "policies ok"
