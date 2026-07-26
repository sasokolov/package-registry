#!/usr/bin/env bash
# Geo scenario 4: a partition must not stop either site from serving, and
# both must converge after healing without operator action.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

stamp="$(date +%s)"
tok_eu="$(geo_token eu "ci-part-eu-$stamp")"
tok_us="$(geo_token us "ci-part-us-$stamp")"
EU_PATH="com/example/parteu/1.0.0/parteu-1.0.0.jar"
US_PATH="com/example/partus/1.0.0/partus-1.0.0.jar"

echo "--> partitioning the sites"
partition
sleep 2

echo "--> each site keeps accepting writes to the active-active feed"
code="$(publish eu shared "$EU_PATH" "eu during partition" "$tok_eu")"
[[ "$code" == "201" ]] || { echo "eu publish during partition: $code" >&2; heal; exit 1; }
code="$(publish us shared "$US_PATH" "us during partition" "$tok_us")"
[[ "$code" == "201" ]] || { echo "us publish during partition: $code" >&2; heal; exit 1; }

echo "--> each site keeps serving its own content"
[[ "$(body eu shared "$EU_PATH")" == "eu during partition" ]] || { heal; exit 1; }
[[ "$(body us shared "$US_PATH")" == "us during partition" ]] || { heal; exit 1; }

echo "--> replication lag is visible while partitioned (not silently ignored)"
lagged() { # <eu|us>: the peer poll is failing and the status says so
  local out
  out="$(repl_status "$1")" || return 1
  grep -qE 'unreachable|timeout|no route|refused' <<<"$out"
}
if ! wait_for 60 lagged eu; then
  echo "eu does not report the partition in repl status" >&2
  repl_status eu >&2
  heal
  exit 1
fi

echo "--> healing"
heal

echo "--> both sites converge on both artifacts, without operator action"
for site in eu us; do
  for pair in "$EU_PATH:eu during partition" "$US_PATH:us during partition"; do
    path="${pair%%:*}"
    want="${pair#*:}"
    if ! wait_for 90 replicated "$site" shared "$path" "$want"; then
      echo "site $site never converged on $path" >&2
      repl_status "$site" >&2
      exit 1
    fi
  done
done

echo "--> the replication status is healthy again"
healthy() { # <eu|us>
  local out
  out="$(repl_status "$1")" || return 1
  ! grep -qE 'unreachable|refused' <<<"$out"
}
for site in eu us; do
  if ! wait_for 60 healthy "$site"; then
    echo "site $site still reports a broken peer after healing" >&2
    repl_status "$site" >&2
    exit 1
  fi
done

echo "partition and heal ok"
