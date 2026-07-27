#!/usr/bin/env bash
# Chaos: a configuration change made through the API on ONE replica must
# reach the others. With the document in the blob store that is the whole
# point of the design; a change visible only where it was made would be
# worse than no API at all.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

API=http://lb/api/v1
admin="$(registry_token "ops-chaos-$(date +%s)")"
FEED="replicated-config"

cleanup() {
  client_curl -sS -o /dev/null -X DELETE -H "Authorization: Bearer $admin" \
    "$API/config/feeds/$FEED" 2>/dev/null || true
}
trap cleanup EXIT

echo "--> creating a feed through whichever replica the balancer picks"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' -X PUT \
  -H "Authorization: Bearer $admin" -H 'Content-Type: application/json' \
  --data "{\"name\":\"$FEED\",\"format\":\"maven\",\"upstream\":\"http://fake-upstream/maven\",\"anonymous\":true}" \
  "$API/config/feeds/$FEED")"
[[ "$code" == "200" || "$code" == "201" ]] || { echo "creation returned $code" >&2; exit 1; }

echo "--> every replica serves it, not just the one that was written to"
# Ask each replica directly: through the balancer a single healthy replica
# could answer every request and hide the others being stale.
replicas="$(compose ps -q registry)"
count="$(grep -c . <<<"$replicas")"
if (( count < 2 )); then
  echo "expected two replicas, found $count" >&2
  exit 1
fi

serves_on() { # <container id>
  docker exec "$1" wget -q -O /dev/null \
    "http://127.0.0.1:8080/maven/$FEED/com/example/liba/1.0.0/liba-1.0.0.jar" 2>/dev/null
}

while read -r replica; do
  [[ -n "$replica" ]] || continue
  if ! wait_for_chaos 90 serves_on "$replica"; then
    echo "replica ${replica:0:12} never picked up the configuration change" >&2
    docker exec "$replica" wget -q -O - --header="Authorization: Bearer $admin" \
      "http://127.0.0.1:8080/api/v1/status" >&2 || true
    exit 1
  fi
  echo "    ${replica:0:12} serves it"
done <<<"$replicas"

echo "--> and the version is the same everywhere (one document, not two)"
versions=""
while read -r replica; do
  [[ -n "$replica" ]] || continue
  v="$(docker exec "$replica" wget -q -O - --header="Authorization: Bearer $admin" \
    "http://127.0.0.1:8080/api/v1/status" 2>/dev/null |
    grep -o '"config_version":"[^"]*"' | cut -d'"' -f4)"
  versions="$versions $v"
done <<<"$replicas"
unique="$(tr ' ' '\n' <<<"$versions" | grep -c . || true)"
distinct="$(tr ' ' '\n' <<<"$versions" | grep . | sort -u | wc -l)"
if [[ "$distinct" != "1" ]]; then
  echo "replicas disagree about the configuration version:$versions" >&2
  exit 1
fi
echo "    both replicas on the same version ($unique checked)"

echo "config reaches every replica"
