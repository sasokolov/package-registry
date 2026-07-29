#!/usr/bin/env bash
# Geo scenario 10: an operator takes a package down at one site. The block
# must reach every site (invariant 14: replication carries decisions that
# REMOVE access), and lifting it must reach them too.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

stamp="$(date +%s)"
token="$(geo_token eu "ci-quar-$stamp")"
PATH_JAR="com/example/quar/1.0.0/quar-1.0.0.jar"
COORD="maven:com.example:quar@1.0.0"

echo "--> publishing and letting both sites converge"
code="$(publish eu homed "$PATH_JAR" "quarantine me" "$token")"
[[ "$code" == "201" ]] || { echo "publish returned $code" >&2; exit 1; }
for site in eu us; do
  if ! wait_for 90 replicated "$site" homed "$PATH_JAR" "quarantine me"; then
    echo "site $site never received the artifact" >&2
    exit 1
  fi
done

echo "--> an operator quarantines the coordinate at eu-1"
compose exec -T registry-eu fondaco repl quarantine \
  -feed homed -coordinate "$COORD" -reason manual -detail "legal takedown" \
  -config /etc/fondaco/config.yaml >/dev/null

echo "--> eu-1 stops serving it"
blocked() { # <eu|us>
  local status
  read -r status _ <<<"$(fetch "$1" homed "$PATH_JAR")"
  [[ "$status" == "409" ]]
}
if ! wait_for 60 blocked eu; then
  echo "the quarantining site still serves the coordinate" >&2
  exit 1
fi

echo "--> and so does us-1, once the decision replicates"
if ! wait_for 90 blocked us; then
  echo "the takedown never reached us-1" >&2
  compose exec -T registry-us fondaco repl status -config /etc/fondaco/config.yaml >&2 || true
  exit 1
fi

echo "--> a manual takedown is not reported as a federation conflict"
conflict_header="$(geo_curl -sS -o /dev/null -w '%header{x-registry-conflict}' \
  "$(site_url us)/maven/homed/$PATH_JAR")"
if [[ -n "$conflict_header" ]]; then
  echo "a manual takedown set X-Registry-Conflict: $conflict_header" >&2
  exit 1
fi

echo "--> every response says which site answered"
site_header="$(geo_curl -sS -o /dev/null -w '%header{x-registry-site}' \
  "$(site_url us)/maven/homed/$PATH_JAR")"
if [[ "$site_header" != "us-1" ]]; then
  echo "the 409 does not carry X-Registry-Site: got '$site_header'" >&2
  exit 1
fi

echo "--> releasing at us-1 reaches eu-1 as well"
compose exec -T registry-us fondaco repl release \
  -feed homed -coordinate "$COORD" -reason manual \
  -config /etc/fondaco/config.yaml >/dev/null

servable() { # <eu|us>
  local status
  read -r status _ <<<"$(fetch "$1" homed "$PATH_JAR")"
  [[ "$status" == "200" ]]
}
for site in us eu; do
  if ! wait_for 90 servable "$site"; then
    echo "site $site still blocks the coordinate after the release" >&2
    compose exec -T "registry-$site" fondaco repl status -config /etc/fondaco/config.yaml >&2 || true
    exit 1
  fi
done

echo "quarantine replication ok"
