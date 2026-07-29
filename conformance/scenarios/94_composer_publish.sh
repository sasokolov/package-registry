#!/usr/bin/env bash
# Phase 13: hosting Composer packages.
#
# Composer has no publish command, so the registry defines the smallest
# convention that could work: PUT the dist archive where it will be served
# from. Everything Composer then reads is derived from the archive, and the
# proof is that a real `composer install` resolves it.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

BASE=http://registry:8080
HOSTED=$BASE/composer/composer-hosted
GROUP=$BASE/composer/composer-public
ci="$(registry_token "ci-composer-$(date +%s)")"

# The archive is a real Composer dist: the same fixture the fake upstream
# serves, uploaded here under a version the upstream does not have.
FIXTURE=/fixtures/composer-upstream/dists/acme/lib-a-f5bcc6039df4.zip

echo "--> uploading a dist archive to the hosted feed"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' -X PUT \
  -H "Authorization: Bearer $ci" --data-binary "@$FIXTURE" \
  "$HOSTED/packages/acme/lib-a/9.9.9.zip")"
[[ "$code" == "201" ]] || { echo "upload returned $code" >&2; exit 1; }

echo "--> the archive is downloadable exactly where it was uploaded"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' "$HOSTED/packages/acme/lib-a/9.9.9.zip")"
[[ "$code" == "200" ]] || { echo "the uploaded archive returned $code" >&2; exit 1; }

echo "--> the root manifest tells Composer where to look packages up"
root="$(client_curl -fsS "$HOSTED/packages.json")"
grep -q 'registry:8080/composer/composer-hosted/p2/%package%.json' <<<"$root" || {
  echo "$root" >&2; exit 1; }
grep -q '"acme/lib-a"' <<<"$root" || { echo "$root" >&2; exit 1; }

echo "--> the p2 document points at this registry and carries the manifest"
p2="$(client_curl -fsS "$HOSTED/p2/acme/lib-a.json")"
grep -q '"version": "9.9.9"' <<<"$p2" || { echo "$p2" >&2; exit 1; }
grep -q 'registry:8080/composer/composer-hosted/packages/acme/lib-a/9.9.9.zip' <<<"$p2" || {
  echo "$p2" >&2; exit 1; }
grep -q '"version_normalized": "9.9.9.0"' <<<"$p2" || { echo "$p2" >&2; exit 1; }

echo "--> an upload whose archive disagrees with its path is refused"
body="$(client_curl -sS -w '\n%{http_code}' -X PUT \
  -H "Authorization: Bearer $ci" --data-binary "@$FIXTURE" \
  "$HOSTED/packages/other/name/1.0.0.zip")"
code="$(tail -n1 <<<"$body")"
[[ "$code" == "400" ]] || { echo "a mismatched upload returned $code, want 400" >&2; exit 1; }

echo "--> re-uploading the same version with different bytes is refused"
body="$(client_curl -sS -w '\n%{http_code}' -X PUT \
  -H "Authorization: Bearer $ci" --data-binary 'not the same archive' \
  "$HOSTED/packages/acme/lib-a/9.9.9.zip")"
code="$(tail -n1 <<<"$body")"
[[ "$code" == "400" || "$code" == "409" ]] || {
  echo "re-upload returned $code" >&2; exit 1; }

echo "--> uploading without the right to publish is refused"
plain="$(registry_token "plain-composer-$(date +%s)")"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' -X PUT \
  -H "Authorization: Bearer $plain" --data-binary "@$FIXTURE" \
  "$HOSTED/packages/acme/lib-a/8.8.8.zip")"
[[ "$code" == "403" ]] || { echo "an identity without publish rights got $code, want 403" >&2; exit 1; }

echo "--> composer install resolves the hosted package"
out="$(compose run --rm -T composer-client sh -c "
  set -e
  rm -rf /tmp/h && mkdir -p /tmp/h && cd /tmp/h
  cat > composer.json <<'JSON'
{
  \"repositories\": [{\"type\": \"composer\", \"url\": \"$HOSTED\"}],
  \"require\": {\"acme/lib-a\": \"9.9.9\"},
  \"config\": {\"secure-http\": false}
}
JSON
  composer install --no-interaction --no-progress
  test -f vendor/acme/lib-a/composer.json
" 2>&1)" || { echo "$out" | tail -30; exit 1; }

echo "--> the group does not claim its hosted member's inventory is everything"
# available-packages is a promise of completeness. A hosted feed can make it;
# a proxy cannot, and publishes no such list. Inheriting the hosted member's
# would make composer refuse to look up anything else — "could not be found
# in any version", for a package sitting one member away.
group_root="$(client_curl -fsS "$GROUP/packages.json")"
if grep -q '"available-packages"' <<<"$group_root"; then
  echo "the group claims a complete inventory it does not have:" >&2
  echo "$group_root" >&2; exit 1
fi
# The hosted feed alone still enumerates: that promise is true of it.
hosted_root="$(client_curl -fsS "$HOSTED/packages.json")"
grep -q '"available-packages"' <<<"$hosted_root" || {
  echo "the hosted feed stopped enumerating what it holds:" >&2
  echo "$hosted_root" >&2; exit 1; }

echo "--> the group shows the local version AND the upstream one"
p2="$(client_curl -fsS "$GROUP/p2/acme/lib-a.json")"
grep -q '"1.0.0"' <<<"$p2" || {
  echo "the upstream version vanished from the group:" >&2; echo "$p2" >&2; exit 1; }
grep -q '"9.9.9"' <<<"$p2" || {
  echo "the local version vanished from the group:" >&2; echo "$p2" >&2; exit 1; }
headers="$(client_curl -sS -o /dev/null -D - "$GROUP/p2/acme/lib-a.json")"
grep -qi '^x-registry-merged: composer-hosted,packagist' <<<"$headers" || { echo "$headers" >&2; exit 1; }

echo "--> and composer installs either of them through the group"
for version in 9.9.9 1.0.0; do
  out="$(compose run --rm -T composer-client sh -c "
    set -e
    rm -rf /tmp/g && mkdir -p /tmp/g && cd /tmp/g
    cat > composer.json <<'JSON'
{
  \"repositories\": [{\"type\": \"composer\", \"url\": \"$GROUP\"}],
  \"require\": {\"acme/lib-a\": \"$version\"},
  \"config\": {\"secure-http\": false}
}
JSON
    composer install --no-interaction --no-progress
    test -f vendor/acme/lib-a/composer.json
  " 2>&1)" || { echo "installing $version through the group failed:" >&2; echo "$out" | tail -30; exit 1; }
done

echo "--> the root manifest tells Composer that search exists"
root="$(client_curl -fsS "$HOSTED/packages.json")"
grep -q 'registry:8080/composer/composer-hosted/search.json' <<<"$root" || {
  echo "the root manifest does not advertise search:" >&2; echo "$root" >&2; exit 1; }

echo "--> search finds what the hosted feed holds"
results="$(client_curl -fsS "$HOSTED/search.json?q=lib-a")"
grep -q '"acme/lib-a"' <<<"$results" || {
  echo "search did not find the uploaded package:" >&2; echo "$results" >&2; exit 1; }
grep -q '"total": 1' <<<"$results" || { echo "$results" >&2; exit 1; }

echo "--> a term that matches nothing is an empty answer, not an error"
results="$(client_curl -fsS "$HOSTED/search.json?q=nothingliketh1s")"
grep -q '"total": 0' <<<"$results" || { echo "$results" >&2; exit 1; }

echo "--> and composer search finds it too"
out="$(compose run --rm -T composer-client sh -c "
  set -e
  rm -rf /tmp/s && mkdir -p /tmp/s && cd /tmp/s
  cat > composer.json <<'JSON'
{
  \"repositories\": [{\"type\": \"composer\", \"url\": \"$HOSTED\"}],
  \"config\": {\"secure-http\": false}
}
JSON
  composer search lib-a --no-interaction 2>&1
" 2>&1)" || { echo "$out" | tail -20; exit 1; }
grep -q "acme/lib-a" <<<"$out" || {
  echo "composer search found nothing:" >&2; echo "$out" | tail -20 >&2; exit 1; }

echo "--> the group's search covers its members"
headers="$(client_curl -sS -o /dev/null -D - "$GROUP/search.json?q=lib-a")"
grep -qi '^x-registry-merged:' <<<"$headers" || {
  echo "the group answered a search from one member only:" >&2; echo "$headers" >&2; exit 1; }

echo "--> publishing to the group is refused, naming the hosted member"
body="$(client_curl -sS -w '\n%{http_code}' -X PUT \
  -H "Authorization: Bearer $ci" --data-binary 'x' \
  "$GROUP/packages/acme/lib-a/7.7.7.zip")"
code="$(tail -n1 <<<"$body")"
[[ "$code" == "405" ]] || { echo "publish to the group returned $code, want 405" >&2; exit 1; }
grep -q 'composer-hosted' <<<"$body" || { echo "the refusal does not name where to publish: $body" >&2; exit 1; }

echo "OK: composer publish"
