#!/usr/bin/env bash
# Geo scenario 1: publish at eu-1 -> install at us-1 both BEFORE convergence
# (served from the peer) and AFTER (served locally).
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

token="$(geo_token eu "ci-geo-$(date +%s)")"
[[ "$token" == reg_* ]] || { echo "token creation failed: ${token:0:12}" >&2; exit 1; }

PATH_JAR=com/example/geo/1.0.0/geo-1.0.0.jar
CONTENT="published at eu-1"

echo "--> publishing at eu-1"
code="$(publish eu homed "$PATH_JAR" "$CONTENT" "$token")"
[[ "$code" == "201" ]] || { echo "publish returned $code" >&2; exit 1; }

echo "--> us-1 serves it (peer fallback while replication catches up, or replicated)"
read -r status source <<<"$(fetch us homed "$PATH_JAR")"
if [[ "$status" != "200" ]]; then
  echo "us-1 returned $status" >&2
  exit 1
fi
case "$source" in
  peer|cache|local) ;;
  *) echo "unexpected source $source" >&2; exit 1 ;;
esac
echo "    served from: $source"

got="$(body us homed "$PATH_JAR")"
[[ "$got" == "$CONTENT" ]] || { echo "content mismatch: $got" >&2; exit 1; }

echo "--> after replication us-1 serves it from its own storage"
locally_cached() {
  local s src
  read -r s src <<<"$(fetch us homed "$PATH_JAR")"
  [[ "$s" == "200" && "$src" == "cache" ]]
}
if ! wait_for 60 locally_cached; then
  echo "us-1 never served the artifact from its own cache" >&2
  repl_status us >&2
  exit 1
fi

echo "--> the generated index at us-1 lists the version (rebuilt locally)"
index_lists_version() {
  local out
  out="$(body us homed com/example/geo/maven-metadata.xml)" || return 1
  [[ "$out" == *"<version>1.0.0</version>"* ]]
}
if ! wait_for 60 index_lists_version; then
  echo "us-1 index does not list the replicated version" >&2
  body us homed com/example/geo/maven-metadata.xml >&2 || true
  exit 1
fi

echo "publish replication ok"
