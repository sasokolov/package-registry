#!/usr/bin/env bash
# Two-site geo conformance: bring up both sites, run every scenario, report.
set -euo pipefail

GEO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export GEO_DIR
export COMPOSE_PROJECT="${COMPOSE_PROJECT:-registry-geo}"

# shellcheck source=lib.sh
source "$GEO_DIR/lib.sh"

KEEP_STACK="${KEEP_STACK:-0}"

cleanup() {
  heal >/dev/null 2>&1 || true
  if [[ "$KEEP_STACK" == "1" ]]; then
    echo "==> KEEP_STACK=1: geo stack left running"
    return
  fi
  compose down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "==> building and starting both sites"
if ! compose up -d --build --wait --wait-timeout 300; then
  compose ps --all
  compose logs --tail 60
  exit 1
fi
compose --profile tools build >/dev/null 2>&1 || true

echo "==> waiting for the peer handshake"
# Note: the status output is captured before matching. Piping a long-running
# `compose exec` into `grep -q` makes grep exit early, the writer takes
# SIGPIPE and pipefail turns a successful check into a failure.
site_ready() { # <eu|us>: the site answers repl status
  local out
  out="$(repl_status "$1")" || return 1
  [[ "$out" == site* ]]
}
for site in eu us; do
  if ! wait_for 60 site_ready "$site"; then
    echo "site $site never reported replication status" >&2
    compose logs --tail 40 "registry-$site"
    exit 1
  fi
done

peer_reachable() { # <eu|us>: a peer poll has succeeded at least once
  local out
  out="$(repl_status "$1")" || return 1
  ! grep -qE '^[a-z].*never' <<<"$out"
}
for site in eu us; do
  if ! wait_for 90 peer_reachable "$site"; then
    echo "site $site never reached its peer" >&2
    repl_status "$site" >&2
    exit 1
  fi
done

declare -a results=()
failures=0
for scenario in "$GEO_DIR"/scenarios/*.sh; do
  name="$(basename "$scenario")"
  echo "==> geo scenario: $name"
  if bash "$scenario"; then
    results+=("PASS  $name")
  else
    results+=("FAIL  $name")
    failures=$((failures + 1))
  fi
done

echo
echo "==> geo conformance report"
printf '%s\n' "${results[@]}"
echo

if (( failures > 0 )); then
  echo "==> site logs (last 60 lines)"
  compose logs --tail 60 registry-eu registry-us
  echo "FAILED: $failures of ${#results[@]} scenario(s)"
  exit 1
fi
echo "OK: all ${#results[@]} geo scenario(s) passed"
