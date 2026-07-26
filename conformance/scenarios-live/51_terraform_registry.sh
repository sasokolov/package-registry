#!/usr/bin/env bash
# LIVE: proxy a real module from registry.terraform.io — versions document
# and archive download (follows the real X-Terraform-Get indirection, which
# points at an absolute external URL). Protocol-level checks via curl: a
# full `terraform init` would additionally download providers, which is out
# of scope for the registry under test.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

MODULE=terraform-aws-modules/kms/aws

echo "--> versions document through the registry"
versions="$(client_curl -fsS "http://registry:8080/terraform/tf/v1/modules/$MODULE/versions")"
grep -q '"versions"' <<<"$versions" || { echo "no versions in response" >&2; exit 1; }
version="$(client_curl -fsS "http://registry:8080/terraform/tf/v1/modules/$MODULE/versions" \
  | tr ',' '\n' | grep -o '"version":"[0-9.]*"' | head -1 | cut -d'"' -f4)"
if [[ -z "$version" ]]; then
  echo "could not extract a version" >&2
  exit 1
fi
echo "    using version $version"

echo "--> download indirection: public registry modules are VCS-backed (git::)"
# registry.terraform.io answers X-Terraform-Get with a git:: source for
# community modules; a pull-through HTTP cache cannot proxy VCS sources and
# must answer with a clean 502 (documented limitation), never a hang or 5xx
# crash. Private registries returning HTTP archives are covered by the
# hermetic scenario 12.
code="$(client_curl -sS -o /dev/null -w '%{http_code}' \
  "http://registry:8080/terraform/tf/v1/modules/$MODULE/$version/archive.tar.gz")"
if [[ "$code" != "502" ]]; then
  echo "VCS-backed module returned $code, want clean 502" >&2
  exit 1
fi

echo "--> versions document is cached (SWR) for the follow-up request"
src="$(client_curl -fsS -o /dev/null -w '%header{x-registry-source}' \
  "http://registry:8080/terraform/tf/v1/modules/$MODULE/versions")"
if [[ "$src" != "cache" ]]; then
  echo "versions source = $src, want cache" >&2
  exit 1
fi

echo "live terraform ok"
