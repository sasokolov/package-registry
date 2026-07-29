#!/usr/bin/env bash
# Phase 12: hosting NuGet packages.
#
# `dotnet nuget push` puts a package here, `dotnet restore` takes it back
# out, and a group over [hosted, proxy] shows both the locally published
# version and the upstream one under the same package id.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

BASE=http://registry:8080
HOSTED=$BASE/nuget/nuget-hosted
GROUP=$BASE/nuget/nuget-public
ci="$(fondaco_token "ci-nuget-$(date +%s)")"

echo "--> the hosted feed advertises where to push; the proxy does not"
index="$(client_curl -fsS "$HOSTED/v3/index.json")"
grep -q '"PackagePublish/2.0.0"' <<<"$index" || {
  echo "the hosted service index has no push endpoint:" >&2; echo "$index" >&2; exit 1; }
grep -q "registry:8080/nuget/nuget-hosted/api/v2/package" <<<"$index" || {
  echo "$index" >&2; exit 1; }
index="$(client_curl -fsS "$BASE/nuget/nugetorg/v3/index.json")"
if grep -q 'PackagePublish' <<<"$index"; then
  echo "a proxy feed advertises a push endpoint it cannot serve:" >&2; echo "$index" >&2; exit 1
fi

echo "--> dotnet pack + dotnet nuget push"
out="$(compose run --rm -T dotnet-client sh -c "
  set -e
  rm -rf /tmp/lib && cp -r /src-lib /tmp/lib && cd /tmp/lib
  dotnet pack -c Release -o /tmp/out >/dev/null
  ls /tmp/out
  dotnet nuget push /tmp/out/conformance.lib.9.9.9.nupkg \
    --source $HOSTED/v3/index.json --api-key '$ci'
" 2>&1)" || { echo "$out" | tail -30; exit 1; }
grep -qi "pushed\|created" <<<"$out" || { echo "$out" | tail -30; exit 1; }

echo "--> the flat container lists the pushed version"
versions="$(client_curl -fsS "$HOSTED/v3/flat2/conformance.lib/index.json")"
grep -q '"9.9.9"' <<<"$versions" || { echo "$versions" >&2; exit 1; }

echo "--> the package itself is downloadable at the path clients ask for"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' \
  "$HOSTED/v3/flat2/conformance.lib/9.9.9/conformance.lib.9.9.9.nupkg")"
[[ "$code" == "200" ]] || { echo "the pushed package returned $code" >&2; exit 1; }
code="$(client_curl -sS -o /dev/null -w '%{http_code}' \
  "$HOSTED/v3/flat2/conformance.lib/9.9.9/conformance.lib.nuspec")"
[[ "$code" == "200" ]] || { echo "the manifest returned $code" >&2; exit 1; }

echo "--> the registration document points back at this registry"
reg="$(client_curl -fsS "$HOSTED/v3/registration/conformance.lib/index.json")"
grep -q "registry:8080/nuget/nuget-hosted/v3/flat2/conformance.lib/9.9.9" <<<"$reg" || {
  echo "registration does not point at the registry:" >&2; echo "$reg" >&2; exit 1; }
grep -q '"licenseExpression": "MIT"' <<<"$reg" || { echo "$reg" >&2; exit 1; }

echo "--> dotnet restore takes the package back out of the hosted feed"
out="$(compose run --rm -T dotnet-client sh -c "
  set -e
  rm -rf /tmp/app && cp -r /src-app /tmp/app && cd /tmp/app
  dotnet restore -p:LibVersion=9.9.9 --source $HOSTED/v3/index.json --no-cache
  test -d packages/conformance.lib/9.9.9
" 2>&1)" || { echo "$out" | tail -30; exit 1; }

echo "--> pushing the same version again is refused: a release is immutable"
out="$(compose run --rm -T dotnet-client sh -c "
  rm -rf /tmp/lib2 && cp -r /src-lib /tmp/lib2 && cd /tmp/lib2
  echo '// changed' >> Thing.cs
  dotnet pack -c Release -o /tmp/out2 >/dev/null
  dotnet nuget push /tmp/out2/conformance.lib.9.9.9.nupkg \
    --source $HOSTED/v3/index.json --api-key '$ci'
" 2>&1)" && { echo "republishing a version succeeded:" >&2; echo "$out" | tail -20 >&2; exit 1; }
grep -qiE "409|conflict|already" <<<"$out" || {
  echo "republish failed for the wrong reason:" >&2; echo "$out" | tail -20 >&2; exit 1; }

echo "--> publishing without the right to publish is refused"
plain="$(fondaco_token "plain-nuget-$(date +%s)")"
out="$(compose run --rm -T dotnet-client sh -c "
  dotnet nuget push /tmp/out/conformance.lib.9.9.9.nupkg \
    --source $HOSTED/v3/index.json --api-key '$plain'
" 2>&1)" && { echo "an identity without publish rights pushed:" >&2; echo "$out" >&2; exit 1; }

echo "--> the group shows the local version AND the upstream one"
versions="$(client_curl -fsS "$GROUP/v3/flat2/conformance.lib/index.json")"
grep -q '"1.2.3"' <<<"$versions" || {
  echo "the upstream version vanished from the group:" >&2; echo "$versions" >&2; exit 1; }
grep -q '"9.9.9"' <<<"$versions" || {
  echo "the local version vanished from the group:" >&2; echo "$versions" >&2; exit 1; }
headers="$(client_curl -sS -o /dev/null -D - "$GROUP/v3/flat2/conformance.lib/index.json")"
grep -qi '^x-registry-merged: nuget-hosted,nugetorg' <<<"$headers" || { echo "$headers" >&2; exit 1; }

echo "--> and dotnet restores either of them through the group"
for version in 9.9.9 1.2.3; do
  out="$(compose run --rm -T dotnet-client sh -c "
    set -e
    rm -rf /tmp/g$version && cp -r /src-app /tmp/g$version && cd /tmp/g$version
    dotnet restore -p:LibVersion=$version --source $GROUP/v3/index.json --no-cache
    test -d packages/conformance.lib/$version
  " 2>&1)" || { echo "restoring $version through the group failed:" >&2; echo "$out" | tail -30; exit 1; }
done

echo "--> search finds what the hosted feed holds"
results="$(client_curl -fsS "$HOSTED/v3/query?q=conformance")"
grep -q '"conformance.lib"' <<<"$results" || {
  echo "search did not find the pushed package:" >&2; echo "$results" >&2; exit 1; }
grep -q '"version": "9.9.9"' <<<"$results" || { echo "$results" >&2; exit 1; }
grep -q '"totalHits": 1' <<<"$results" || { echo "$results" >&2; exit 1; }

echo "--> a term that matches nothing is an empty answer, not an error"
results="$(client_curl -fsS "$HOSTED/v3/query?q=nothingliketh1s")"
grep -q '"totalHits": 0' <<<"$results" || { echo "$results" >&2; exit 1; }

echo "--> the query reaches the feed: two searches are two answers"
one="$(client_curl -fsS "$HOSTED/v3/query?q=conformance")"
two="$(client_curl -fsS "$HOSTED/v3/query?q=nothingliketh1s")"
[[ "$one" != "$two" ]] || {
  echo "two different searches returned the same answer; the query is being dropped" >&2
  exit 1; }

echo "--> and the group's search covers its members"
results="$(client_curl -fsS "$GROUP/v3/query?q=conformance")"
grep -q '"conformance.lib"' <<<"$results" || { echo "$results" >&2; exit 1; }
headers="$(client_curl -sS -o /dev/null -D - "$GROUP/v3/query?q=conformance")"
grep -qi '^x-registry-merged:' <<<"$headers" || {
  echo "the group answered a search from one member only:" >&2; echo "$headers" >&2; exit 1; }

echo "--> publishing to the group is refused, naming the hosted member"
body="$(client_curl -sS -w '\n%{http_code}' -X PUT \
  -H "Authorization: Bearer $ci" --data-binary 'x' "$GROUP/api/v2/package")"
code="$(tail -n1 <<<"$body")"
[[ "$code" == "405" ]] || { echo "publish to the group returned $code, want 405" >&2; exit 1; }
grep -q 'nuget-hosted' <<<"$body" || { echo "the refusal does not name where to publish: $body" >&2; exit 1; }

echo "OK: nuget publish"
