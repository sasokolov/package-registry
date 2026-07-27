#!/usr/bin/env bash
# Chaos: kill -9 one of two registry replicas WHILE `npm ci` is downloading.
# The install must still succeed — the load balancer retries against the
# surviving replica, and replicas share nothing that would break
# (invariant 3).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

CLIENT_NAME="chaos-npm-$$"

cleanup() {
  docker rm -f "$CLIENT_NAME" >/dev/null 2>&1 || true
  compose up -d --wait registry >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Note: capture, then take the first line. Piping `compose ps` into `head`
# closes the pipe early, the writer takes SIGPIPE, and pipefail turns that
# into a failure that `set -e` swallows silently — which made this scenario
# flaky rather than wrong.
replicas="$(compose ps -q registry)"
victim="$(head -1 <<<"$replicas")"
if [[ -z "$victim" ]]; then
  echo "no registry replica found" >&2
  exit 1
fi
count="$(grep -c . <<<"$replicas")"
if (( count < 2 )); then
  echo "the chaos stack must run two replicas; found $count" >&2
  exit 1
fi

echo "--> starting npm ci against the load balancer"
compose run --rm -d -T --name "$CLIENT_NAME" npm-client sh -c '
  set -e
  cp -r /src /tmp/w && cd /tmp/w
  npm config set registry http://lb/npm/npmjs/
  npm config set fetch-retries 5
  npm ci --no-audit --no-fund > /tmp/out 2>&1
  echo DONE >> /tmp/out
  sleep 120
' >/dev/null

# Kill only once the install is genuinely in flight. The client writes its
# output at the end, so the signal is the REGISTRY seeing tarball requests:
# waiting on the client's log would mean killing after the install.
echo "--> waiting for the install to actually start fetching tarballs"
in_flight() {
  local logs
  logs="$(compose logs --since 120s registry 2>/dev/null)" || return 1
  grep -qE '"path":"/npm/npmjs/[^"]*\.tgz"' <<<"$logs"
}
if ! wait_for_chaos 90 in_flight; then
  echo "npm never started fetching tarballs" >&2
  compose logs --tail 20 registry >&2 || true
  exit 1
fi

docker kill -s KILL "$victim" >/dev/null
echo "    killed replica ${victim:0:12} mid-install"

echo "--> the install still completes through the surviving replica"
finished() {
  docker exec "$CLIENT_NAME" grep -q DONE /tmp/out 2>/dev/null
}
if ! wait_for_chaos 180 finished; then
  echo "npm ci did not finish after the replica kill; client output:" >&2
  docker exec "$CLIENT_NAME" cat /tmp/out 2>/dev/null | tail -20 >&2 || true
  exit 1
fi

echo "--> the surviving replica keeps serving"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' http://lb/npm/npmjs/left-pad)"
if [[ "$code" != "200" ]]; then
  echo "the surviving replica returned $code" >&2
  exit 1
fi

echo "replica kill survived"
