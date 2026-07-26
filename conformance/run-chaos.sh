#!/usr/bin/env bash
# Chaos conformance: two registry replicas behind a load balancer, then
# failures injected while real clients are working.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export COMPOSE_PROJECT="${COMPOSE_PROJECT:-registry-chaos}"
export COMPOSE_OVERLAY="$SCRIPT_DIR/compose.chaos.yml"
export SCENARIO_DIR="$SCRIPT_DIR/scenarios-chaos"

exec "$SCRIPT_DIR/run.sh"
