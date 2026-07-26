#!/usr/bin/env bash
# anonymous: false feed: 401 without credentials, 200 with a valid static
# token created via `registry token create`. The blob endpoint requires
# authentication too.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

URL=http://registry:8080/maven/private/com/example/liba/1.0.0/liba-1.0.0.jar

code="$(client_curl -sS -o /dev/null -w '%{http_code}' "$URL")"
if [[ "$code" != "401" ]]; then
  echo "anonymous request returned $code, want 401" >&2
  exit 1
fi
www="$(client_curl -sS -o /dev/null -w '%header{www-authenticate}' "$URL")"
if [[ -z "$www" ]]; then
  echo "401 without WWW-Authenticate" >&2
  exit 1
fi

echo "--> creating a static token via the CLI"
secret="$(compose exec -T registry registry token create \
  -name "conformance-$(date +%s)" -config /etc/registry/config.yaml 2>/dev/null | tail -1)"
if [[ "$secret" != reg_* ]]; then
  echo "token create did not return a secret (got: ${secret:0:8}...)" >&2
  exit 1
fi

code="$(client_curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $secret" "$URL")"
if [[ "$code" != "200" ]]; then
  echo "authenticated request returned $code, want 200" >&2
  exit 1
fi

echo "--> blob endpoint: 401 anonymous, 200 with token"
digest="$(sha256sum "$CONFORMANCE_DIR/fixtures/maven/com/example/liba/1.0.0/liba-1.0.0.jar" | cut -d' ' -f1)"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' "http://registry:8080/-/blobs/sha256/$digest")"
if [[ "$code" != "401" ]]; then
  echo "anonymous blob access returned $code, want 401" >&2
  exit 1
fi
code="$(client_curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $secret" \
  "http://registry:8080/-/blobs/sha256/$digest")"
if [[ "$code" != "200" ]]; then
  echo "authenticated blob access returned $code, want 200" >&2
  exit 1
fi

echo "auth token ok"
