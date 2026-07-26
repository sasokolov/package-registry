#!/usr/bin/env bash
# Hermetic conformance run: build and start the compose stack, wait for every
# service to become healthy, execute all scenarios, print a report, tear the
# stack down. Exits non-zero if any scenario fails.
#
# KEEP_STACK=1 ./conformance/run.sh   leaves the stack running for debugging.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export CONFORMANCE_DIR="$SCRIPT_DIR"
export COMPOSE_PROJECT="${COMPOSE_PROJECT:-registry-conformance}"

# shellcheck source=lib.sh
source "$SCRIPT_DIR/lib.sh"

KEEP_STACK="${KEEP_STACK:-0}"

cleanup() {
  if [[ "$KEEP_STACK" == "1" ]]; then
    echo "==> KEEP_STACK=1: stack left running (compose project $COMPOSE_PROJECT)"
    return
  fi
  compose down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "==> building and starting the stack"
if ! compose up -d --build --wait --wait-timeout 300; then
  echo "==> stack failed to become healthy; service status and logs:"
  compose ps --all
  compose logs --tail 100
  exit 1
fi

declare -a results=()
failures=0
for scenario in "$SCRIPT_DIR"/scenarios/*.sh; do
  name="$(basename "$scenario")"
  echo "==> scenario: $name"
  if bash "$scenario"; then
    results+=("PASS  $name")
  else
    results+=("FAIL  $name")
    failures=$((failures + 1))
  fi
done

echo
echo "==> conformance report"
printf '%s\n' "${results[@]}"
echo

if (( failures > 0 )); then
  echo "==> service logs (last 50 lines each)"
  compose logs --tail 50
  echo "FAILED: $failures of ${#results[@]} scenario(s)"
  exit 1
fi
echo "OK: all ${#results[@]} scenario(s) passed"
