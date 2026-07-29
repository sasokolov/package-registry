#!/usr/bin/env bash
# Phase 4 acceptance: a repeated `npm ci` must be served almost entirely
# from cache — cache hit ratio for the npm feed > 0.95 in the metrics.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

metric() { # <source>
  local value
  value="$(client_curl -fsS http://registry:8080/metrics |
    grep -E "^fondaco_requests_total\{feed=\"npmjs\",source=\"$1\"\}" | awk '{print $2}')"
  printf '%.0f' "${value:-0}"
}

before_cache="$(metric cache)"
before_upstream="$(metric upstream)"
before_stale="$(metric stale)"

echo "--> warm run (populates the cache)"
compose run --rm -T npm-client sh -c '
  set -e
  cp -r /src /tmp/w1 && cd /tmp/w1
  npm config set registry http://registry:8080/npm/npmjs/
  npm ci --no-audit --no-fund >/dev/null
' >/dev/null

mid_cache="$(metric cache)"
mid_upstream="$(metric upstream)"
mid_stale="$(metric stale)"

echo "--> measured run (everything must come from cache)"
compose run --rm -T npm-client sh -c '
  set -e
  cp -r /src /tmp/w2 && cd /tmp/w2
  npm config set registry http://registry:8080/npm/npmjs/
  npm ci --no-audit --no-fund >/dev/null
' >/dev/null

after_cache="$(metric cache)"
after_upstream="$(metric upstream)"
after_stale="$(metric stale)"

hits=$(( after_cache - mid_cache ))
misses=$(( (after_upstream - mid_upstream) + (after_stale - mid_stale) ))
total=$(( hits + misses ))
echo "    requests during the measured run: hits=$hits misses=$misses (baseline $before_cache/$before_upstream/$before_stale)"

if (( total == 0 )); then
  echo "no npm feed requests were recorded" >&2
  exit 1
fi
# ratio > 0.95 <=> hits*100 > total*95
if (( hits * 100 <= total * 95 )); then
  echo "cache hit ratio $hits/$total is not above 0.95" >&2
  exit 1
fi

echo "npm cache hit ratio ok ($hits/$total)"
