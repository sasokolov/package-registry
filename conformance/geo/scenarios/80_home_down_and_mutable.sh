#!/usr/bin/env bash
# Geo scenario 11: two behaviours the design leans on and nothing covered.
#
#  1. A publish to a feed whose home site is unreachable answers 503 with a
#     pointer — never a redirect, never a silent queue (invariant 4).
#  2. Concurrent updates of a MUTABLE coordinate (an npm dist-tag) on both
#     sides of a partition converge on one value. Every other conflict test
#     uses immutable jars, i.e. the K1 branch; this is the HLC branch, which
#     is the entire reason the hybrid clock and migration 0004 exist.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

stamp="$(date +%s)"
tok_eu="$(geo_token eu "ci-home-eu-$stamp")"
tok_us="$(geo_token us "ci-home-us-$stamp")"

trap 'heal >/dev/null 2>&1 || true' EXIT

echo "--> partitioning, so the home site of the forward-homed feed is unreachable"
partition
sleep 3

echo "--> publishing to a forward-homed feed from the non-home site"
resp="$(geo_curl -sS -o /dev/null \
  -w '%{http_code}|%header{retry-after}|%header{x-registry-home-site}|%header{x-registry-site}' \
  -X PUT --data-binary "orphan" -H "Authorization: Bearer $tok_us" \
  "$(site_url us)/maven/homed/com/example/orphan/1.0.0/orphan-1.0.0.jar")"
IFS='|' read -r code retry home site <<<"$resp"

if [[ "$code" != "503" ]]; then
  echo "publish with the home site down returned $code, want 503" >&2
  exit 1
fi
if [[ -z "$retry" ]]; then
  echo "the 503 carries no Retry-After" >&2
  exit 1
fi
if [[ "$home" != "eu-1" ]]; then
  echo "the 503 does not name the home site: '$home'" >&2
  exit 1
fi
if [[ "$site" != "us-1" ]]; then
  echo "the 503 does not name the answering site: '$site'" >&2
  exit 1
fi
echo "    503, Retry-After: $retry, home: $home, answered by: $site"

echo "--> nothing was queued: the coordinate does not exist anywhere"
for s in us eu; do
  read -r status _ <<<"$(fetch "$s" homed "com/example/orphan/1.0.0/orphan-1.0.0.jar")"
  if [[ "$status" == "200" ]]; then
    echo "site $s serves an artifact whose publish was refused" >&2
    exit 1
  fi
done

echo "--> both sides move the same npm dist-tag while partitioned"
npm_publish() { # <site> <version>
  compose run --rm -T -e NPM_TOKEN="$2" npm-client sh -c "
    set -e
    mkdir -p /tmp/p-$3 && cd /tmp/p-$3
    cat > package.json <<JSON
{\"name\": \"tagged-pkg\", \"version\": \"$3\", \"main\": \"index.js\", \"license\": \"MIT\"}
JSON
    echo 'module.exports = \"$3\";' > index.js
    npm config set registry http://registry-$1:8080/npm/shared-npm/
    npm config set //registry-$1:8080/npm/shared-npm/:_authToken \$NPM_TOKEN
    npm publish --tag latest
  " 2>&1
}

# Fresh versions per run: a coordinate is immutable, so a repeat run must
# not collide with the previous one.
V_EU="1.0.$((stamp % 100000))"
V_US="2.0.$((stamp % 100000))"
out_eu="$(npm_publish eu "$tok_eu" "$V_EU")" || { echo "$out_eu" | tail -10; exit 1; }
out_us="$(npm_publish us "$tok_us" "$V_US")" || { echo "$out_us" | tail -10; exit 1; }

echo "--> healing"
heal

echo "--> both sites resolve the tag to the same version"
tag_of() { # <eu|us>
  local body
  body="$(compose run --rm -T npm-client sh -c \
    "wget -qO- http://registry-$1:8080/npm/shared-npm/tagged-pkg 2>/dev/null")" || return 1
  # The document is pretty-printed, so the pattern tolerates whitespace.
  grep -o '"latest"[[:space:]]*:[[:space:]]*"[^"]*"' <<<"$body" | head -1 |
    grep -o '[0-9][^"]*'
}
converged() {
  local a b
  a="$(tag_of eu)"; b="$(tag_of us)"
  [[ -n "$a" && "$a" == "$b" ]]
}
if ! wait_for 120 converged; then
  echo "the sites disagree about the dist-tag: eu=$(tag_of eu) us=$(tag_of us)" >&2
  compose exec -T registry-eu registry repl status -config /etc/registry/config.yaml >&2 || true
  exit 1
fi
echo "    both sites: $(tag_of eu)"

echo "--> and both versions are still installable (nothing was lost)"
for version in "$V_EU" "$V_US"; do
  for site in eu us; do
    if ! wait_for 60 bash -c "true"; then :; fi
    body="$(compose run --rm -T npm-client sh -c \
      "wget -qO- http://registry-$site:8080/npm/shared-npm/tagged-pkg 2>/dev/null")"
    if ! grep -q "\"$version\"" <<<"$body"; then
      echo "site $site lost version $version" >&2
      exit 1
    fi
  done
done

echo "home-down publish and mutable convergence ok"
