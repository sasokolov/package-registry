#!/usr/bin/env bash
# Chaos: object storage goes away. Cached metadata keeps serving from the
# in-process view where it can, publishing fails loudly rather than
# half-committing, and everything recovers when storage returns.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

restore_storage() {
  compose up -d --wait minio >/dev/null 2>&1 || true
}
trap restore_storage EXIT

token="$(fondaco_token "ci-s3-$(date +%s)")"
HOSTED=http://lb/maven/hosted/com/example/s3out/1.0.0/s3out-1.0.0.jar

echo "--> publishing works while storage is up"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' -X PUT --data-binary 'before' \
  -H "Authorization: Bearer $token" "$HOSTED")"
[[ "$code" == "201" ]] || { echo "publish before the outage returned $code" >&2; exit 1; }

echo "--> stopping object storage"
compose stop minio >/dev/null

echo "--> publishing fails loudly instead of half-committing"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' -X PUT --data-binary 'during' \
  -H "Authorization: Bearer $token" \
  "http://lb/maven/hosted/com/example/s3out/2.0.0/s3out-2.0.0.jar")"
if [[ "$code" == "201" ]]; then
  echo "publish reported success with no storage to write to" >&2
  exit 1
fi
if [[ "$code" != "500" && "$code" != "502" && "$code" != "503" ]]; then
  echo "publish during a storage outage returned $code, want a 5xx" >&2
  exit 1
fi
echo "    publish -> $code"

echo "--> reads fail with a 5xx too, not a 404 (a missing backend is not a missing package)"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' "$HOSTED")"
if [[ "$code" == "404" ]]; then
  echo "a storage outage was reported to the client as 'package not found'" >&2
  exit 1
fi
echo "    read -> $code"

echo "--> the process stays alive and keeps answering probes"
for _ in 1 2 3; do
  code="$(client_curl -sS -o /dev/null -w '%{http_code}' http://lb/healthz)"
  [[ "$code" == "200" ]] || { echo "healthz returned $code during a storage outage" >&2; exit 1; }
  sleep 1
done

echo "--> restoring storage"
compose up -d --wait minio >/dev/null

echo "--> serving recovers without a restart"
recovered() {
  local code
  code="$(client_curl -sS -o /dev/null -w '%{http_code}' "$HOSTED")"
  [[ "$code" == "200" ]]
}
if ! wait_for_chaos 120 recovered; then
  echo "the registry did not recover after storage came back" >&2
  exit 1
fi

echo "--> and the half-published coordinate did not survive as a phantom"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' \
  "http://lb/maven/hosted/com/example/s3out/2.0.0/s3out-2.0.0.jar")"
if [[ "$code" == "200" ]]; then
  echo "the publish that failed during the outage is being served" >&2
  exit 1
fi

echo "storage outage survived"
