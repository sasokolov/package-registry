#!/usr/bin/env bash
# Geo scenario 8: a cursor that falls behind the peer's retained journal
# must recover by itself. The peer answers 410 and the site re-bootstraps
# from a snapshot instead of silently missing events.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

stamp="$(date +%s)"
token="$(geo_token eu "ci-gc-$stamp")"
PATH_JAR="com/example/gcresync/1.0.0/gcresync-1.0.0.jar"
CONTENT="published before the journal was pruned"

psql_eu() { compose exec -T postgres-eu psql -U registry -d registry -tA -c "$1"; }

echo "--> publishing at eu-1 and waiting for us-1 to converge"
code="$(publish eu homed "$PATH_JAR" "$CONTENT" "$token")"
[[ "$code" == "201" ]] || { echo "publish returned $code" >&2; exit 1; }
if ! wait_for 90 replicated us homed "$PATH_JAR" "$CONTENT"; then
  echo "us-1 never received the artifact" >&2
  exit 1
fi

echo "--> rewinding the us-1 cursor and pruning the eu-1 journal past it"
compose exec -T registry-us registry repl resync -peer eu-1 \
  -config /etc/registry/config.yaml >/dev/null
head="$(psql_eu "SELECT COALESCE(MAX(origin_seq),0) FROM repl_journal WHERE origin_site='eu-1'")"
if [[ "${head:-0}" -lt 2 ]]; then
  echo "eu-1 journal is too short to prune (head=$head)" >&2
  exit 1
fi
# Simulate retention-driven GC: everything below the head is gone, so the
# rewound cursor now points before the oldest retained entry.
psql_eu "DELETE FROM repl_journal WHERE origin_site='eu-1' AND origin_seq < $head" >/dev/null

echo "--> us-1 detects the gap and bootstraps from a snapshot"
bootstrapped() {
  local logs
  logs="$(compose logs --since 120s --no-log-prefix registry-us 2>/dev/null)" || return 1
  grep -q 'bootstrapping from peer snapshot' <<<"$logs"
}
if ! wait_for 90 bootstrapped; then
  echo "us-1 never re-bootstrapped after the journal gap" >&2
  compose exec -T registry-us registry repl status -config /etc/registry/config.yaml >&2 || true
  exit 1
fi

echo "--> the artifact is still served at us-1 after the resync"
if ! wait_for 60 replicated us homed "$PATH_JAR" "$CONTENT"; then
  echo "us-1 lost the artifact across the resync" >&2
  exit 1
fi

echo "--> new publishes keep flowing after the resync"
NEW_PATH="com/example/gcresync/2.0.0/gcresync-2.0.0.jar"
code="$(publish eu homed "$NEW_PATH" "published after the resync" "$token")"
[[ "$code" == "201" ]] || { echo "post-resync publish returned $code" >&2; exit 1; }
if ! wait_for 90 replicated us homed "$NEW_PATH" "published after the resync"; then
  echo "replication did not resume after the resync" >&2
  compose exec -T registry-us registry repl status -config /etc/registry/config.yaml >&2 || true
  exit 1
fi

echo "journal GC and automatic resync ok"
