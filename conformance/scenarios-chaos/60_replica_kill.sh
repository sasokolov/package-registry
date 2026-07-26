#!/usr/bin/env bash
# Chaos: kill -9 one of two registry replicas in the middle of `npm ci`.
# The install must still succeed — the load balancer retries against the
# surviving replica, and replicas share nothing that would break (invariant 3).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

restore_replicas() {
  compose up -d --wait registry >/dev/null 2>&1 || true
}
trap restore_replicas EXIT

victim="$(compose ps -q registry | head -1)"
if [[ -z "$victim" ]]; then
  echo "no registry replica found" >&2
  exit 1
fi

echo "--> starting npm ci against the load balancer and killing a replica mid-flight"
compose run --rm -d -T npm-client sh -c '
  set -e
  cp -r /src /tmp/w && cd /tmp/w
  npm config set registry http://lb/npm/npmjs/
  npm config set fetch-retries 5
  npm ci --no-audit --no-fund > /tmp/out 2>&1
  echo DONE >> /tmp/out
  sleep 60
' >/dev/null

sleep 1
docker kill -s KILL "$victim" >/dev/null
echo "    killed replica ${victim:0:12}"

echo "--> the install still completes through the surviving replica"
deadline=$((SECONDS + 120))
container=""
while (( SECONDS < deadline )); do
  container="$(docker ps -q --filter "ancestor=node:22-alpine" | head -1)"
  [[ -n "$container" ]] && break
  sleep 1
done
if [[ -z "$container" ]]; then
  echo "npm client container not found" >&2
  exit 1
fi

while (( SECONDS < deadline )); do
  if docker exec "$container" grep -q DONE /tmp/out 2>/dev/null; then
    docker rm -f "$container" >/dev/null 2>&1 || true
    echo "replica kill survived"
    exit 0
  fi
  if ! docker ps -q --no-trunc | grep -q "$container"; then
    break
  fi
  sleep 2
done

echo "npm ci did not finish after the replica kill; client output:" >&2
docker exec "$container" cat /tmp/out 2>/dev/null | tail -20 >&2 || true
docker rm -f "$container" >/dev/null 2>&1 || true
exit 1
