#!/usr/bin/env bash
# Phase 13: a Terraform group.
#
# A Terraform module source names a HOST, not a path, and service discovery
# names exactly one registry for that host. So a group is not a convenience
# here — it is the only way a site can serve its own modules and a proxied
# registry at the same address.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

BASE=http://registry:8080
ci="$(fondaco_token "ci-tf-$(date +%s)")"

echo "--> service discovery points at the group, not at one of its members"
discovery="$(client_curl -fsS "$BASE/.well-known/terraform.json")"
grep -q '"/terraform/tf-public/v1/modules/"' <<<"$discovery" || {
  echo "$discovery" >&2; exit 1; }

echo "--> publishing a module version the upstream does not have"
ARCHIVE=/fixtures/terraform-upstream/archives/mymod-2.0.0.tar.gz
code="$(client_curl -sS -o /dev/null -w '%{http_code}' -X PUT \
  -H "Authorization: Bearer $ci" --data-binary "@$ARCHIVE" \
  "$BASE/terraform/tf-hosted/v1/modules/testns/mymod/generic/3.0.0/archive.tar.gz")"
[[ "$code" == "201" ]] || { echo "upload returned $code" >&2; exit 1; }

echo "--> the group lists versions from BOTH members"
versions="$(client_curl -fsS "$BASE/terraform/tf-public/v1/modules/testns/mymod/generic/versions")"
for want in '"1.0.0"' '"2.0.0"' '"3.0.0"'; do
  grep -q "$want" <<<"$versions" || {
    echo "version $want is missing from the merged list:" >&2; echo "$versions" >&2; exit 1; }
done
headers="$(client_curl -sS -o /dev/null -D - \
  "$BASE/terraform/tf-public/v1/modules/testns/mymod/generic/versions")"
grep -qi '^x-registry-merged: tf-hosted,tf' <<<"$headers" || { echo "$headers" >&2; exit 1; }
grep -qi '^x-registry-source: local' <<<"$headers" || { echo "$headers" >&2; exit 1; }

echo "--> archives come from whichever member has them"
headers="$(client_curl -sS -o /dev/null -D - \
  "$BASE/terraform/tf-public/v1/modules/testns/mymod/generic/3.0.0/archive.tar.gz")"
grep -qi '^x-registry-member: tf-hosted' <<<"$headers" || {
  echo "the locally published archive did not come from the hosted member:" >&2
  echo "$headers" >&2; exit 1; }
headers="$(client_curl -sS -o /dev/null -D - \
  "$BASE/terraform/tf-public/v1/modules/testns/mymod/generic/2.0.0/archive.tar.gz")"
grep -qi '^x-registry-member: tf' <<<"$headers" || {
  echo "the upstream archive did not come from the proxy member:" >&2
  echo "$headers" >&2; exit 1; }

echo "--> terraform init resolves the locally published version through discovery"
out="$(compose run --rm -T --entrypoint sh terraform-client -c '
  set -e
  rm -rf /tmp/g && mkdir -p /tmp/g && cd /tmp/g
  cat > main.tf <<TF
module "mymod" {
  source  = "registry.local/testns/mymod/generic"
  version = "3.0.0"
}
TF
  terraform init -no-color
' 2>&1)" || { echo "$out" | tail -40; exit 1; }
grep -q "Terraform has been successfully initialized" <<<"$out" || {
  echo "$out" | tail -20 >&2; exit 1; }

echo "--> and the upstream version too, from the same address"
out="$(compose run --rm -T --entrypoint sh terraform-client -c '
  set -e
  rm -rf /tmp/g2 && mkdir -p /tmp/g2 && cd /tmp/g2
  cat > main.tf <<TF
module "mymod" {
  source  = "registry.local/testns/mymod/generic"
  version = "2.0.0"
}
TF
  terraform init -no-color
' 2>&1)" || { echo "$out" | tail -40; exit 1; }
grep -q "Terraform has been successfully initialized" <<<"$out" || {
  echo "$out" | tail -20 >&2; exit 1; }

echo "--> publishing to the group is refused, naming the hosted member"
body="$(client_curl -sS -w '\n%{http_code}' -X PUT \
  -H "Authorization: Bearer $ci" --data-binary 'x' \
  "$BASE/terraform/tf-public/v1/modules/testns/mymod/generic/4.0.0/archive.tar.gz")"
code="$(tail -n1 <<<"$body")"
[[ "$code" == "405" ]] || { echo "publish to the group returned $code, want 405" >&2; exit 1; }
grep -q 'tf-hosted' <<<"$body" || { echo "the refusal does not name where to publish: $body" >&2; exit 1; }

echo "OK: terraform group"
