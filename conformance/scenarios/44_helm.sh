#!/usr/bin/env bash
# Helm chart repositories, driven by the real helm client: proxying one,
# hosting one in the shape ChartMuseum made standard, and a group over both.
#
# The index is the whole protocol — it is the only document that says what a
# repository has, and every entry carries the URL its archive is at. A proxy
# that forgot to rewrite those URLs would still look right in `helm search`
# and would send every download straight past the cache; a group that took
# the first member's index instead of merging would hide every upstream
# chart behind the hosted one. Both are checked here, because neither shows
# up as an error.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

BASE=http://registry:8080
PROXY=$BASE/helm/charts
HOSTED=$BASE/helm/helm-hosted
GROUP=$BASE/helm/helm-public

token="$(registry_token "ci-helm-$(date +%s)")"

# Every invocation is a fresh container, so anything that depends on a
# previous helm command — and `helm repo add` is nothing but state for the
# next one — has to run inside a single shell.
helm_sh() { compose run --rm -T --entrypoint sh helm-client -c "$1" 2>&1; }

echo "--> the proxied index points at this registry, not at the upstream"
index="$(client_curl -fsS "$PROXY/index.yaml")"
grep -q "upstream-chart" <<<"$index" || { echo "$index" >&2; exit 1; }
grep -q "http://registry:8080/helm/charts/charts/upstream-chart-1.0.0.tgz" <<<"$index" || {
  echo "chart URLs still point at the upstream — every download would miss the cache:" >&2
  echo "$index" >&2; exit 1; }
if grep -q "http://fake-upstream" <<<"$index"; then
  echo "an upstream URL survived the rewrite:" >&2; echo "$index" >&2; exit 1
fi

echo "--> helm itself reads it, and pulls through the cache"
out="$(helm_sh "
  set -e
  helm repo add proxy $PROXY >/dev/null
  helm repo update proxy >/dev/null
  helm search repo proxy
")" || { echo "$out" >&2; exit 1; }
grep -q "proxy/upstream-chart" <<<"$out" || { echo "$out" >&2; exit 1; }

code="$(client_curl -sS -o /dev/null -w '%{http_code}' \
  "$PROXY/charts/upstream-chart-1.0.0.tgz")"
[[ "$code" == "200" ]] || { echo "pulling the proxied chart returned $code" >&2; exit 1; }
# Cached now: the second pull must not reach the upstream.
source_header="$(client_curl -sS -o /dev/null -D - \
  "$PROXY/charts/upstream-chart-1.0.0.tgz" | tr -d '\r' | grep -i '^x-registry-source' | awk '{print $2}')"
[[ "$source_header" == "cache" ]] || {
  echo "the second pull came from $source_header, want cache" >&2; exit 1; }

echo "--> a chart uploads the way ChartMuseum takes it"
# Packaged by helm itself, so what is uploaded is a real chart.
out="$(helm_sh "
  set -e
  cd /tmp
  helm create hosted-chart >/dev/null
  sed -i 's/^version: .*/version: 2.0.0/' hosted-chart/Chart.yaml
  sed -i 's/^description: .*/description: A hosted chart/' hosted-chart/Chart.yaml
  helm package hosted-chart -d /tmp >/dev/null
  wget -q -O- --method=POST --header='Authorization: Bearer $token' \
    --body-file=/tmp/hosted-chart-2.0.0.tgz $HOSTED/api/charts && echo UPLOADED
")" || { echo "$out" >&2; exit 1; }
grep -q UPLOADED <<<"$out" || { echo "$out" >&2; exit 1; }

echo "--> and appears in the index this feed generates"
index="$(client_curl -fsS "$HOSTED/index.yaml")"
grep -q "name: hosted-chart" <<<"$index" || { echo "$index" >&2; exit 1; }
grep -q "version: 2.0.0" <<<"$index" || { echo "$index" >&2; exit 1; }
# The description came from Chart.yaml inside the archive, which is the only
# authority on what a chart is: the file name cannot say.
grep -q "A hosted chart" <<<"$index" || {
  echo "the chart's own metadata is missing from the index:" >&2; echo "$index" >&2; exit 1; }
grep -q "http://registry:8080/helm/helm-hosted/charts/hosted-chart-2.0.0.tgz" <<<"$index" || {
  echo "$index" >&2; exit 1; }

echo "--> ChartMuseum's own listing answers too"
listing="$(client_curl -fsS "$HOSTED/api/charts")"
grep -q '"hosted-chart"' <<<"$listing" || { echo "$listing" >&2; exit 1; }
one="$(client_curl -fsS "$HOSTED/api/charts/hosted-chart")"
grep -q '"version":"2.0.0"' <<<"$one" || { echo "$one" >&2; exit 1; }

echo "--> republishing the same version with different content is refused"
# A fresh container, so the chart is repackaged here with different content
# under the same version — which is exactly what immutability must refuse.
body="$(helm_sh "
  cd /tmp
  helm create hosted-chart >/dev/null
  sed -i 's/^version: .*/version: 2.0.0/' hosted-chart/Chart.yaml
  sed -i 's/^description: .*/description: Something else entirely/' hosted-chart/Chart.yaml
  helm package hosted-chart -d /tmp >/dev/null
  wget -q -S -O- --method=POST --header='Authorization: Bearer $token' \
    --body-file=/tmp/hosted-chart-2.0.0.tgz $HOSTED/api/charts 2>&1 | head -5
" || true)"
grep -qE "409|Conflict" <<<"$body" || {
  echo "a chart version was overwritten (invariant 4):" >&2; echo "$body" >&2; exit 1; }

echo "--> deleting a published version is refused, with somewhere to go"
body="$(client_curl -sS -w '\n%{http_code}' -X DELETE -H "Authorization: Bearer $token" \
  "$HOSTED/api/charts/hosted-chart/2.0.0")"
code="$(tail -n1 <<<"$body")"
[[ "$code" == "403" ]] || { echo "delete returned $code, want 403" >&2; echo "$body" >&2; exit 1; }
grep -qi "quarantine" <<<"$body" || {
  echo "the refusal does not say what to do instead:" >&2; echo "$body" >&2; exit 1; }

echo "--> the group shows the hosted chart AND the proxied one"
index="$(client_curl -fsS "$GROUP/index.yaml")"
grep -q "name: hosted-chart" <<<"$index" || {
  echo "the hosted chart is missing from the group:" >&2; echo "$index" >&2; exit 1; }
grep -q "name: upstream-chart" <<<"$index" || {
  echo "the upstream chart vanished behind the hosted member:" >&2; echo "$index" >&2; exit 1; }
headers="$(client_curl -sS -o /dev/null -D - "$GROUP/index.yaml" | tr -d '\r')"
grep -qi '^x-registry-merged: helm-hosted,charts' <<<"$headers" || { echo "$headers" >&2; exit 1; }

echo "--> and helm installs either of them through the group"
out="$(helm_sh "
  set -e
  helm repo add public $GROUP >/dev/null
  helm repo update public >/dev/null
  helm template r1 public/hosted-chart --version 2.0.0 | head -8
  helm pull public/upstream-chart --version 1.0.0 -d /tmp
  ls /tmp/upstream-chart-1.0.0.tgz
")" || { echo "$out" >&2; exit 1; }
grep -q "upstream-chart-1.0.0.tgz" <<<"$out" || { echo "$out" >&2; exit 1; }
grep -q "kind: ServiceAccount" <<<"$out" || {
  echo "the hosted chart did not render through the group:" >&2; echo "$out" >&2; exit 1; }

echo "--> publishing without the right is refused"
nobody="$(registry_token "nobody-helm-$(date +%s)")"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $nobody" \
  --data-binary @/fixtures/helm-upstream/charts/upstream-chart-1.0.0.tgz "$HOSTED/api/charts")"
[[ "$code" == "403" ]] || { echo "an identity without publish rights got $code" >&2; exit 1; }

echo "OK: helm"
