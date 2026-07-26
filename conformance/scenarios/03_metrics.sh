#!/usr/bin/env bash
# After the echo scenarios, /metrics must expose per-feed pipeline metrics:
# requests by source (cache hits included) and upstream latency histograms.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

metrics="$(client_curl -fsS http://registry:8080/metrics)"

require() {
  if ! grep -qE "$1" <<<"$metrics"; then
    echo "metric missing: $1" >&2
    exit 1
  fi
}

require 'registry_requests_total\{feed="test",source="cache"\} [1-9]'
require 'registry_requests_total\{feed="test",source="upstream"\} [1-9]'
require 'registry_requests_total\{feed="test",source="stale"\} [1-9]'
require 'registry_upstream_requests_total\{feed="test",outcome="ok"\} [1-9]'
require 'registry_upstream_request_duration_seconds_count\{feed="test"\} [1-9]'
require 'registry_upstream_breaker_state\{feed="test"\}'

echo "metrics ok"
