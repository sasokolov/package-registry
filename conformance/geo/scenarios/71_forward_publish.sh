#!/usr/bin/env bash
# Geo scenario 2: write-affinity. A publish sent to us-1 for a feed homed at
# eu-1 is forwarded there, so the immutability check stays authoritative.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

token="$(geo_token us "ci-fwd-$(date +%s)")"
PATH_JAR=com/example/fwd/1.0.0/fwd-1.0.0.jar

echo "--> publishing at us-1 (feed is homed at eu-1)"
code="$(publish us homed "$PATH_JAR" "forwarded content" "$token")"
if [[ "$code" != "201" ]]; then
  echo "forwarded publish returned $code, want 201" >&2
  exit 1
fi

echo "--> the artifact exists at the home site"
at_home() { [[ "$(body eu homed "$PATH_JAR")" == "forwarded content" ]]; }
if ! wait_for 30 at_home; then
  echo "home site does not have the forwarded artifact" >&2
  exit 1
fi

echo "--> the audit trail names the real publisher, not the forwarding site"
logs="$(compose logs --no-log-prefix registry-eu)"
audit="$(grep '"log":"audit"' <<<"$logs" | grep 'package published' || true)"
grep -q 'com.example:fwd' <<<"$audit" || { echo "no publish audit at the home site" >&2; exit 1; }

echo "--> republishing the same coordinate with different content is 409"
code="$(publish us homed "$PATH_JAR" "different bytes" "$token")"
if [[ "$code" != "409" ]]; then
  echo "conflicting forwarded publish returned $code, want 409" >&2
  exit 1
fi

echo "publish forwarding ok"
