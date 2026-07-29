#!/usr/bin/env bash
# Acceptance tests for the Terraform provider against a real registry.
#
# The registry is the conformance stack's own: same binary, same admin API,
# same validation. A mock would prove nothing here, because the whole point of
# the provider is that the registry decides what a valid configuration is.
#
# KEEP_STACK=1 ./conformance/run-terraform.sh   leaves the stack up.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export CONFORMANCE_DIR="$SCRIPT_DIR"
export COMPOSE_PROJECT="${COMPOSE_PROJECT:-registry-terraform}"
export COMPOSE_OVERLAY="$SCRIPT_DIR/compose.terraform.yml"

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

echo "==> starting the registry"
if ! compose up -d --build --wait --wait-timeout 300 registry minio postgres fake-upstream; then
  compose ps --all
  compose logs --tail 100
  exit 1
fi

echo "==> building the acceptance runner"
compose --profile tools build tf-acc

echo "==> minting an administrator token"
token="$(fondaco_token "ops-terraform")"
[[ -n "$token" ]] || { echo "no token was issued" >&2; exit 1; }

echo "==> terraform acceptance tests"
compose run --rm -T \
  -e FONDACO_TOKEN="$token" \
  tf-acc test ./... -run 'TestAcc' -count=1 -v -timeout 20m

echo "==> terraform end-to-end (real CLI, real provider binary)"
compose run --rm -T \
  -e FONDACO_TOKEN="$token" \
  --entrypoint /bin/sh \
  tf-acc /src/conformance/terraform/e2e.sh
