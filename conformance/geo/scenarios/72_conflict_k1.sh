#!/usr/bin/env bash
# Geo scenario 3: two sites publish DIFFERENT bytes at the same coordinate
# of an active-active feed. Rule K1 must converge both sites on the same
# canonical digest, quarantine the coordinate, and refuse to serve it until
# an operator resolves the conflict.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

stamp="$(date +%s)"
PATH_JAR="com/example/clash/1.0.0/clash-1.0.0.jar"
COORD="maven:com.example:clash@1.0.0"
tok_eu="$(geo_token eu "ci-clash-eu-$stamp")"
tok_us="$(geo_token us "ci-clash-us-$stamp")"

echo "--> partitioning the sites so both publishes are accepted locally"
partition
sleep 2

code_eu="$(publish eu shared "$PATH_JAR" "content from eu" "$tok_eu")"
code_us="$(publish us shared "$PATH_JAR" "content from us" "$tok_us")"
if [[ "$code_eu" != "201" || "$code_us" != "201" ]]; then
  echo "publishes during the partition returned eu=$code_eu us=$code_us, want 201/201" >&2
  heal
  exit 1
fi

echo "--> healing the partition"
heal

echo "--> both sites detect the conflict and quarantine the coordinate"
conflict_seen() { # <eu|us>
  local out
  out="$(compose exec -T "registry-$1" registry repl conflicts -config /etc/registry/config.yaml 2>/dev/null)" || return 1
  [[ "$out" == *"clash"* ]]
}
for site in eu us; do
  if ! wait_for 90 conflict_seen "$site"; then
    echo "site $site never recorded the conflict" >&2
    repl_status "$site" >&2
    exit 1
  fi
done

echo "--> the conflicted coordinate is not served (quarantine)"
for site in eu us; do
  read -r status _ <<<"$(fetch "$site" shared "$PATH_JAR")"
  # A quarantined coordinate answers 409 (the same status the read path has
  # used for quarantine since Phase 5).
  if [[ "$status" != "409" ]]; then
    echo "site $site served a conflicted coordinate with status $status" >&2
    exit 1
  fi
done

echo "--> both sites agree on the canonical digest (content-derived, not clock-derived)"
digest_of() { # <eu|us>
  compose exec -T "registry-$1" registry repl conflicts -config /etc/registry/config.yaml 2>/dev/null |
    awk '/clash/ {print $3; exit}'
}
d_eu="$(digest_of eu)"
d_us="$(digest_of us)"
if [[ -z "$d_eu" || "$d_eu" != "$d_us" ]]; then
  echo "sites disagree on the canonical digest: eu=$d_eu us=$d_us" >&2
  exit 1
fi
echo "    canonical: $d_eu"

echo "--> an operator resolves the conflict, and every site converges on the choice"
conflicts_json() { # <eu|us>
  compose exec -T "registry-$1" registry repl conflicts -json -config /etc/registry/config.yaml 2>/dev/null
}
json="$(conflicts_json eu)"
canonical="$(grep -o '"canonical_sha256": "[0-9a-f]*"' <<<"$json" | head -1 | grep -o '[0-9a-f]\{64\}')"
other="$(grep -o '"other_sha256": "[0-9a-f]*"' <<<"$json" | head -1 | grep -o '[0-9a-f]\{64\}')"
if [[ ${#canonical} -ne 64 || ${#other} -ne 64 ]]; then
  echo "could not read both digests from the conflict record" >&2
  echo "$json" >&2
  exit 1
fi

# Deliberately keep the OTHER digest: the operator's choice must win over
# the automatic K1 pick, everywhere.
compose exec -T registry-eu registry repl resolve \
  -feed shared -path "$PATH_JAR" -keep "$other" -config /etc/registry/config.yaml >/dev/null

resolved_everywhere() { # <eu|us>
  local out
  out="$(body "$1" shared "$PATH_JAR" 2>/dev/null)" || return 1
  [[ "$out" == "content from eu" || "$out" == "content from us" ]]
}
for site in eu us; do
  if ! wait_for 90 resolved_everywhere "$site"; then
    echo "site $site still refuses to serve the resolved coordinate" >&2
    compose exec -T "registry-$site" registry repl conflicts -config /etc/registry/config.yaml >&2 || true
    exit 1
  fi
done

echo "--> both sites serve identical bytes after the resolution"
b_eu="$(body eu shared "$PATH_JAR")"
b_us="$(body us shared "$PATH_JAR")"
if [[ "$b_eu" != "$b_us" ]]; then
  echo "sites diverged after resolution: eu=$b_eu us=$b_us" >&2
  exit 1
fi

echo "rule K1 conflict handling ok"
