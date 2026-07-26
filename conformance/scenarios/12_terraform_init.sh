#!/usr/bin/env bash
# terraform init of the reference module through the registry (service
# discovery via the TLS gateway, download via X-Terraform-Get pointing back
# at the registry, archive ingested from the fake upstream).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

echo "--> terraform init via https://registry.local"
out="$(compose run --rm -T --entrypoint sh terraform-client -c '
  set -e
  cp -r /src /tmp/w && cd /tmp/w
  terraform init -no-color
  terraform validate -no-color
  grep -q "mymod 2.0.0" .terraform/modules/mymod/main.tf
' 2>&1)" || {
  echo "$out" | tail -40
  exit 1
}
grep -q "Terraform has been successfully initialized" <<<"$out" || {
  echo "terraform init did not succeed:" >&2
  echo "$out" | tail -20 >&2
  exit 1
}

echo "--> module archive is cached (second download served from cache)"
src="$(client_curl -fsS -o /dev/null -w '%header{x-registry-source}' \
  http://registry:8080/terraform/tf/v1/modules/testns/mymod/generic/2.0.0/archive.tar.gz)"
if [[ "$src" != "cache" ]]; then
  echo "archive source = $src, want cache" >&2
  exit 1
fi

echo "terraform init ok"
