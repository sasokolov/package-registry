#!/usr/bin/env bash
# Geo scenario 12: an event this binary cannot apply must park without
# blocking the stream behind it, be visible to an operator, and show up in
# the metrics the alerts watch. Nothing exercised the park path before, and
# no scenario had ever asserted a replication metric.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

stamp="$(date +%s)"
token="$(geo_token us "ci-park-$stamp")"
PATH_JAR="com/example/parked/1.0.0/parked-1.0.0.jar"

metric() { # <eu|us> <metric name>
  compose run --rm -T npm-client sh -c \
    "wget -qO- http://registry-$1:8080/metrics 2>/dev/null" |
    grep "^$2" | head -1
}

echo "--> baseline: the parked gauge is exported and zero"
gauge="$(metric eu registry_repl_parked_events)"
if [[ -z "$gauge" ]]; then
  echo "registry_repl_parked_events is not exported at all" >&2
  exit 1
fi
echo "    $gauge"

echo "--> injecting an event kind this binary does not understand into us-1's journal"
# A newer peer in a mixed-version mesh looks exactly like this.
compose exec -T postgres-us psql -U registry -d registry -tA -c "
  WITH stamp AS (SELECT * FROM repl_hlc_next())
  INSERT INTO repl_journal (origin_site, origin_seq, kind, payload, hlc_wall, hlc_logical)
  SELECT 'us-1', stamp.seq, 'a_future_event_kind', '{\"whatever\": true}'::jsonb,
         stamp.hlc_wall, stamp.hlc_logical FROM stamp" >/dev/null

echo "--> eu-1 parks it instead of stalling"
parked_visible() {
  local out
  out="$(compose exec -T registry-eu registry repl retry-parked -config /etc/registry/config.yaml 2>/dev/null)" || return 1
  grep -q 'a_future_event_kind' <<<"$out"
}
if ! wait_for 90 parked_visible; then
  echo "the unknown event never reached the parked table" >&2
  compose exec -T registry-eu registry repl status -config /etc/registry/config.yaml >&2 || true
  exit 1
fi

echo "--> the operator sees it, with the reason"
report="$(compose exec -T registry-eu registry repl retry-parked -config /etc/registry/config.yaml 2>/dev/null)"
grep -q 'unknown event kind' <<<"$report" || {
  echo "the parked report does not explain why:" >&2; echo "$report" >&2; exit 1; }

echo "--> and the metric the alert watches is non-zero"
parked_metric_set() {
  local value
  value="$(metric eu registry_repl_parked_events | awk '{print $2}')"
  [[ -n "$value" && "$value" != "0" ]]
}
if ! wait_for 90 parked_metric_set; then
  echo "registry_repl_parked_events stayed zero with an event parked" >&2
  metric eu registry_repl_parked_events >&2
  exit 1
fi
echo "    $(metric eu registry_repl_parked_events)"

echo "--> the stream is NOT blocked: a later publish still replicates"
code="$(publish us shared "$PATH_JAR" "published after the parked event" "$token")"
[[ "$code" == "201" ]] || { echo "publish returned $code" >&2; exit 1; }
if ! wait_for 90 replicated eu shared "$PATH_JAR" "published after the parked event"; then
  echo "a parked event blocked everything behind it (head-of-line blocking)" >&2
  compose exec -T registry-eu registry repl status -config /etc/registry/config.yaml >&2 || true
  exit 1
fi

echo "--> lag and applied-event metrics are exported and moving"
for m in registry_repl_lag registry_repl_applied_total registry_repl_feed_digest; do
  if [[ -z "$(metric eu "$m")" ]]; then
    echo "$m is not exported" >&2
    exit 1
  fi
done
echo "    $(metric eu registry_repl_applied_total)"

echo "--> both sites report the same digest for a converged feed"
digest_of() { # <eu|us>
  metric "$1" 'registry_repl_feed_digest{feed="shared"}' | awk '{print $2}'
}
digests_agree() {
  local a b
  a="$(digest_of eu)"; b="$(digest_of us)"
  [[ -n "$a" && "$a" == "$b" ]]
}
if ! wait_for 120 digests_agree; then
  echo "the divergence detector reports different digests: eu=$(digest_of eu) us=$(digest_of us)" >&2
  exit 1
fi
echo "    both sites: $(digest_of eu)"

echo "parked events and metrics ok"
