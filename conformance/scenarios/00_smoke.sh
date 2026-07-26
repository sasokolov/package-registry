#!/usr/bin/env bash
# Smoke: registry health endpoints and the fake-upstream respond from inside
# the compose network.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

body="$(client_curl -fsS http://registry:8080/healthz)"
if [[ "$body" != "ok" ]]; then
  echo "unexpected /healthz body: $body" >&2
  exit 1
fi

client_curl -fsS -o /dev/null http://registry:8080/readyz

metrics="$(client_curl -fsS http://registry:8080/metrics)"
if ! grep -q '^go_goroutines' <<<"$metrics"; then
  echo "/metrics does not expose go_goroutines" >&2
  exit 1
fi

fixture="$(client_curl -fsS http://fake-upstream/smoke.txt)"
if [[ "$fixture" != "fake-upstream smoke fixture" ]]; then
  echo "unexpected fake-upstream fixture body: $fixture" >&2
  exit 1
fi

echo "smoke ok"
