#!/usr/bin/env bash
# Phase 11: a group is one URL over local and proxied content.
#
# The check that matters is not "does it serve" but "does it serve BOTH" —
# a group that answers the version index from its first member looks fine
# until the day someone asks for a version only the other member has.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

BASE=http://registry:8080
ci="$(registry_token "ci-group-$(date +%s)")"

echo "--> publishing a version into the hosted member that the upstream does not have"
# The upload body goes on the command line: the client container cannot see
# files created on the host.
publish() { # <path> <content>
  client_curl -sS -o /dev/null -w '%{http_code}' -X PUT \
    -H "Authorization: Bearer $ci" --data-binary "$2" "$BASE/maven/group-hosted/$1"
}
code="$(publish "com/example/liba/2.0.0/liba-2.0.0.jar" "locally published liba 2.0.0")"
[[ "$code" == "201" ]] || { echo "publishing the jar returned $code" >&2; exit 1; }
code="$(publish "com/example/liba/2.0.0/liba-2.0.0.pom" \
  '<project><modelVersion>4.0.0</modelVersion><groupId>com.example</groupId><artifactId>liba</artifactId><version>2.0.0</version></project>')"
[[ "$code" == "201" ]] || { echo "publishing the pom returned $code" >&2; exit 1; }

echo "--> the group's index lists versions from BOTH members"
index="$(client_curl -sS "$BASE/maven/maven-public/com/example/liba/maven-metadata.xml")"
grep -q '<version>1.0.0</version>' <<<"$index" || {
  echo "the upstream version vanished from the merged index:" >&2; echo "$index" >&2; exit 1; }
grep -q '<version>2.0.0</version>' <<<"$index" || {
  echo "the locally published version vanished from the merged index:" >&2; echo "$index" >&2; exit 1; }
grep -q '<latest>2.0.0</latest>' <<<"$index" || {
  echo "latest was not recomputed across members:" >&2; echo "$index" >&2; exit 1; }

echo "--> and it says which members it was merged from"
headers="$(client_curl -sS -o /dev/null -D - "$BASE/maven/maven-public/com/example/liba/maven-metadata.xml")"
grep -qi '^x-registry-merged: group-hosted,central' <<<"$headers" || { echo "$headers" >&2; exit 1; }
grep -qi '^x-registry-source: local' <<<"$headers" || { echo "$headers" >&2; exit 1; }

echo "--> artifacts come from whichever member has them, and the answer says which"
headers="$(client_curl -sS -o /dev/null -D - \
  "$BASE/maven/maven-public/com/example/liba/1.0.0/liba-1.0.0.jar")"
grep -qiE '^HTTP/[0-9.]+ 200' <<<"$headers" || { echo "$headers" >&2; exit 1; }
grep -qi '^x-registry-member: central' <<<"$headers" || {
  echo "the upstream artifact did not come from the proxy member:" >&2; echo "$headers" >&2; exit 1; }

headers="$(client_curl -sS -o /dev/null -D - \
  "$BASE/maven/maven-public/com/example/liba/2.0.0/liba-2.0.0.jar")"
grep -qiE '^HTTP/[0-9.]+ 200' <<<"$headers" || { echo "$headers" >&2; exit 1; }
grep -qi '^x-registry-member: group-hosted' <<<"$headers" || {
  echo "the local artifact did not come from the hosted member:" >&2; echo "$headers" >&2; exit 1; }

echo "--> a coordinate no member has is a 404 that names the group"
body="$(client_curl -sS -w '\n%{http_code}' \
  "$BASE/maven/maven-public/com/example/nothing/9.9.9/nothing-9.9.9.jar")"
code="$(tail -n1 <<<"$body")"
[[ "$code" == "404" ]] || { echo "missing coordinate returned $code" >&2; exit 1; }
grep -q 'maven-public' <<<"$body" || { echo "the 404 does not name the group: $body" >&2; exit 1; }

echo "--> publishing to a group is refused, with the member to use instead"
body="$(client_curl -sS -w '\n%{http_code}' -X PUT \
  -H "Authorization: Bearer $ci" --data-binary 'x' \
  "$BASE/maven/maven-public/com/example/liba/3.0.0/liba-3.0.0.jar")"
code="$(tail -n1 <<<"$body")"
[[ "$code" == "405" ]] || { echo "publish to a group returned $code, want 405" >&2; exit 1; }
grep -q 'group-hosted' <<<"$body" || { echo "the refusal does not name where to publish: $body" >&2; exit 1; }

echo "--> mvn resolves through the group alone: a range that only the local"
echo "    member can satisfy, and a dependency only the upstream has"
out="$(compose run --rm -T maven-client -B -s /work-group/settings.xml -f /work-group/pom.xml \
  org.apache.maven.plugins:maven-dependency-plugin:3.6.1:resolve 2>&1)" || {
  echo "$out" | tail -40
  exit 1
}
grep -q "BUILD SUCCESS" <<<"$out" || { echo "$out" | tail -40; exit 1; }
grep -q "com.example:liba:jar:2.0.0" <<<"$out" || {
  echo "the range did not resolve to the locally published version:" >&2
  echo "$out" | tail -40; exit 1; }
grep -q "com.example:libb:jar:1.0.0" <<<"$out" || {
  echo "the upstream-only dependency did not resolve through the group:" >&2
  echo "$out" | tail -40; exit 1; }

echo "--> npm: the group's package document is merged and points at the group"
doc="$(client_curl -sS "$BASE/npm/npm-public/left-pad")"
grep -q '"1.3.0"' <<<"$doc" || { echo "the upstream package vanished: $doc" >&2; exit 1; }
grep -q '/npm/npm-public/left-pad/-/left-pad-1.3.0.tgz' <<<"$doc" || {
  echo "the merged document points somewhere other than the group:" >&2; echo "$doc" >&2; exit 1; }

echo "--> npm installs from the group: an upstream package and a local one"
publish_npm="$(compose run --rm -T -e NPM_TOKEN="$ci" npm-client sh -c "
  set -e
  rm -rf /tmp/gp && mkdir -p /tmp/gp && cd /tmp/gp
  cat > package.json <<'JSON'
{ \"name\": \"group-local\", \"version\": \"1.0.0\", \"license\": \"MIT\" }
JSON
  echo 'module.exports = 1;' > index.js
  npm config set registry http://registry:8080/npm/npm-hosted/
  npm config set //registry:8080/npm/npm-hosted/:_authToken \$NPM_TOKEN
  npm publish
" 2>&1)" || { echo "$publish_npm" | tail -20; exit 1; }

out="$(compose run --rm -T npm-client sh -c "
  set -e
  rm -rf /tmp/gi && mkdir -p /tmp/gi && cd /tmp/gi
  npm config set registry http://registry:8080/npm/npm-public/
  npm install --no-audit --no-fund left-pad@1.3.0 group-local@1.0.0
  ls node_modules
" 2>&1)" || { echo "$out" | tail -30; exit 1; }
grep -q "left-pad" <<<"$out" || { echo "$out" | tail -30; exit 1; }
grep -q "group-local" <<<"$out" || { echo "$out" | tail -30; exit 1; }

echo "OK: groups"
