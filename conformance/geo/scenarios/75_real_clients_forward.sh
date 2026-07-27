#!/usr/bin/env bash
# Geo scenario 6: the tools people actually run must work through a
# non-home site — `npm publish` and `mvn deploy` against us-1 for feeds
# homed at eu-1, then `npm install` from us-1 once it has replicated.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

stamp="$(date +%s)"
token="$(geo_token us "ci-clients-$stamp")"
VERSION="1.0.$((stamp % 1000))"

echo "--> npm publish against us-1 (feed homed at eu-1)"
out="$(compose run --rm -T -e NPM_TOKEN="$token" npm-client sh -c "
  set -e
  mkdir -p /tmp/p && cd /tmp/p
  cat > package.json <<'JSON'
{
  \"name\": \"geo-pkg\",
  \"version\": \"$VERSION\",
  \"main\": \"index.js\",
  \"license\": \"MIT\"
}
JSON
  echo 'module.exports = \"geo\";' > index.js
  npm config set registry http://registry-us:8080/npm/npmhosted/
  npm config set //registry-us:8080/npm/npmhosted/:_authToken \$NPM_TOKEN
  npm publish
" 2>&1)" || { echo "$out" | tail -15; exit 1; }
grep -q "geo-pkg" <<<"$out" || { echo "$out" | tail -15; exit 1; }

echo "--> the home site owns the published version"
home_has_it() {
  local out
  out="$(compose run --rm -T npm-client sh -c \
    "wget -qO- http://registry-eu:8080/npm/npmhosted/geo-pkg" 2>/dev/null)" || return 1
  [[ "$out" == *"\"$VERSION\""* ]]
}
if ! wait_for 60 home_has_it; then
  echo "the home site does not list the version published through us-1" >&2
  exit 1
fi

echo "--> us-1 rebuilds its own index for the forwarded publish"
# Generated indexes are derived data, rebuilt locally from the replicated
# manifest set (invariant 15) — they are not themselves replicated, so a
# client at us-1 sees the version once that rebuild has happened.
us_lists_it() {
  local out
  out="$(compose run --rm -T npm-client sh -c \
    "wget -qO- http://registry-us:8080/npm/npmhosted/geo-pkg" 2>/dev/null)" || return 1
  [[ "$out" == *"\"$VERSION\""* ]]
}
if ! wait_for 90 us_lists_it; then
  echo "us-1 never rebuilt its index for the forwarded publish" >&2
  compose exec -T registry-us registry repl status -config /etc/registry/config.yaml >&2 || true
  exit 1
fi

echo "--> npm install from us-1 resolves it (peer fallback or replicated)"
out="$(compose run --rm -T npm-client sh -c "
  set -e
  mkdir -p /tmp/i && cd /tmp/i
  npm config set registry http://registry-us:8080/npm/npmhosted/
  npm install geo-pkg@$VERSION --no-audit --no-fund
  node -e \"require('geo-pkg'); console.log('INSTALL_OK')\"
" 2>&1)" || { echo "$out" | tail -15; exit 1; }
grep -q INSTALL_OK <<<"$out" || { echo "$out" | tail -15; exit 1; }

echo "--> mvn deploy against us-1 (feed homed at eu-1)"
out="$(compose run --rm -T -e REGISTRY_TOKEN="$token" --entrypoint sh maven-client -c "
  set -e
  cp -r /work-geo /tmp/m && cd /tmp/m
  sed -i 's/GEO_VERSION/$VERSION/' pom.xml
  mvn -q -B -s settings.xml \
    -Ddeploy.url=http://registry-us:8080/maven/homed \
    deploy
  echo DEPLOY_OK
" 2>&1)" || { echo "$out" | tail -25; exit 1; }
grep -q DEPLOY_OK <<<"$out" || { echo "$out" | tail -25; exit 1; }

echo "--> the forwarded maven artifact is at the home site"
mvn_at_home() {
  local out
  out="$(compose run --rm -T npm-client sh -c \
    "wget -qO- http://registry-eu:8080/maven/homed/com/example/geo-deployed/maven-metadata.xml" 2>/dev/null)" || return 1
  [[ "$out" == *"$VERSION"* ]]
}
if ! wait_for 60 mvn_at_home; then
  echo "the home site does not list the deployed version" >&2
  exit 1
fi

echo "real clients through a non-home site ok"
