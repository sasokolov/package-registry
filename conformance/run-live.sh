#!/usr/bin/env bash
# Conformance against REAL upstreams (Maven Central, registry.terraform.io).
# Manual run: needs internet access; not part of `make conformance`.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export COMPOSE_PROJECT="${COMPOSE_PROJECT:-registry-live}"
export COMPOSE_OVERLAY="$SCRIPT_DIR/compose.live.yml"
export SCENARIO_DIR="$SCRIPT_DIR/scenarios-live"

exec "$SCRIPT_DIR/run.sh"
