#!/usr/bin/env bash
# Geo scenario 9: a site loses its database entirely (the disaster the
# runbook calls "вывод/возврат peer'а"). Rebuilding it from empty gives it a
# NEW site UUID, which peers must refuse until an operator explicitly says
# it is the same site — and once they do, it must converge back on its own.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

stamp="$(date +%s)"
token="$(geo_token eu "ci-dr-$stamp")"
PATH_JAR="com/example/dr/1.0.0/dr-1.0.0.jar"
CONTENT="published before the site was lost"

echo "--> publishing at eu-1 and letting us-1 converge"
code="$(publish eu homed "$PATH_JAR" "$CONTENT" "$token")"
[[ "$code" == "201" ]] || { echo "publish returned $code" >&2; exit 1; }
if ! wait_for 90 replicated us homed "$PATH_JAR" "$CONTENT"; then
  echo "us-1 never received the artifact" >&2
  exit 1
fi

old_uuid="$(compose exec -T registry-us fondaco repl status \
  -config /etc/fondaco/config.yaml 2>/dev/null | awk 'NR==1 {print $3}' | tr -d '()')"
echo "    us-1 identity: $old_uuid"

echo "--> losing us-1's database and storage entirely"
compose stop registry-us >/dev/null
compose rm -fsv postgres-us minio-us >/dev/null 2>&1
compose up -d --wait postgres-us minio-us >/dev/null
compose up -d --wait registry-us >/dev/null

echo "--> the rebuilt site has a new identity"
new_uuid=""
identity_ready() {
  local out
  out="$(compose exec -T registry-us fondaco repl status -config /etc/fondaco/config.yaml 2>/dev/null)" || return 1
  new_uuid="$(awk 'NR==1 {print $3}' <<<"$out" | tr -d '()')"
  [[ -n "$new_uuid" && "$new_uuid" != "$old_uuid" ]]
}
if ! wait_for 90 identity_ready; then
  echo "the rebuilt site did not come up with a new identity" >&2
  exit 1
fi
echo "    new identity: $new_uuid"

echo "--> eu-1 refuses to replicate from a site whose identity changed"
refused() {
  local logs
  logs="$(compose logs --since 90s --no-log-prefix registry-eu 2>/dev/null)" || return 1
  grep -q 'identity changed' <<<"$logs"
}
if ! wait_for 90 refused; then
  echo "eu-1 kept replicating from a site whose UUID changed" >&2
  compose exec -T registry-eu fondaco repl status -config /etc/fondaco/config.yaml >&2 || true
  exit 1
fi

echo "--> the refusal is visible to an operator, not just in the log"
status="$(compose exec -T registry-eu fondaco repl status -config /etc/fondaco/config.yaml 2>/dev/null)"
grep -q 'PEER IDENTITY MISMATCH' <<<"$status" || {
  echo "repl status does not mention the identity problem:" >&2
  echo "$status" >&2
  exit 1; }

echo "--> after an explicit trust reset, eu-1 accepts the rebuilt site"
compose exec -T registry-eu fondaco repl trust-reset -peer us-1 \
  -config /etc/fondaco/config.yaml >/dev/null

healthy_again() {
  local out
  out="$(compose exec -T registry-eu fondaco repl status -config /etc/fondaco/config.yaml 2>/dev/null)" || return 1
  ! grep -qi 'identity changed' <<<"$out"
}
if ! wait_for 90 healthy_again; then
  echo "eu-1 still refuses the peer after the trust reset" >&2
  compose exec -T registry-eu fondaco repl status -config /etc/fondaco/config.yaml >&2 || true
  exit 1
fi

echo "--> the rebuilt site converges back on its own"
if ! wait_for 180 replicated us homed "$PATH_JAR" "$CONTENT"; then
  echo "the rebuilt site never recovered the artifact" >&2
  compose exec -T registry-us fondaco repl status -config /etc/fondaco/config.yaml >&2 || true
  exit 1
fi

echo "--> and takes new publishes again"
NEW_PATH="com/example/dr/2.0.0/dr-2.0.0.jar"
code="$(publish eu homed "$NEW_PATH" "published after recovery" "$token")"
[[ "$code" == "201" ]] || { echo "post-recovery publish returned $code" >&2; exit 1; }
if ! wait_for 120 replicated us homed "$NEW_PATH" "published after recovery"; then
  echo "replication did not resume after the recovery" >&2
  exit 1
fi

echo "site loss and recovery ok"
