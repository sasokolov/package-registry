#!/usr/bin/env bash
# Geo scenario 5: revoking a token propagates to every site (invariant 14:
# replication may only REMOVE authority).
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

name="ci-revoke-$(date +%s)"
token="$(geo_token eu "$name")"
PATH_A="com/example/revoke/1.0.0/revoke-1.0.0.jar"
PATH_B="com/example/revoke/2.0.0/revoke-2.0.0.jar"

echo "--> the token works at its own site"
code="$(publish eu shared "$PATH_A" "before revocation" "$token")"
[[ "$code" == "201" ]] || { echo "publish before revocation: $code" >&2; exit 1; }

echo "--> revoking it at eu-1"
compose exec -T registry-eu registry token revoke -name "$name" \
  -config /etc/registry/config.yaml >/dev/null

echo "--> eu-1 rejects it within the revocation sweep window"
# Verified identities are cached so the read path survives a database
# outage (invariant 7); revocation therefore takes effect within
# auth.revocation_sweep (5s by default), not instantly.
revoked_at_eu() {
  local code
  code="$(publish eu shared "$PATH_B" "after revocation" "$token")"
  [[ "$code" == "401" || "$code" == "403" ]]
}
if ! wait_for 30 revoked_at_eu; then
  echo "revoked token still works at its own site" >&2
  exit 1
fi

echo "--> us-1 rejects it once the revocation replicates"
revoked_at_us() {
  local code
  code="$(publish us shared "$PATH_B" "after revocation" "$token")"
  [[ "$code" == "401" || "$code" == "403" ]]
}
if ! wait_for 90 revoked_at_us; then
  echo "us-1 still accepts the revoked token" >&2
  repl_status us >&2
  exit 1
fi

echo "--> a token CREATED at eu-1 does not appear at us-1"
# Replication may only remove authority, never grant it (invariant 14):
# there is no token-create event in the protocol.
fresh_name="ci-localonly-$(date +%s)"
fresh="$(geo_token eu "$fresh_name")"
code="$(publish eu shared "com/example/localonly/1.0.0/localonly-1.0.0.jar" "local" "$fresh")"
[[ "$code" == "201" ]] || { echo "fresh token does not work at its own site: $code" >&2; exit 1; }

still_rejected_at_us() {
  local code
  code="$(publish us shared "com/example/localonly/2.0.0/localonly-2.0.0.jar" "local" "$fresh")"
  [[ "$code" == "401" || "$code" == "403" ]]
}
# Give replication several poll cycles: the point is that it never grants
# this token, not that it is merely slow.
for _ in 1 2 3 4 5 6; do
  if ! still_rejected_at_us; then
    echo "a token created at eu-1 became valid at us-1: replication granted authority" >&2
    exit 1
  fi
  sleep 2
done

echo "token revocation replication ok"
