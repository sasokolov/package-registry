#!/usr/bin/env bash
# Phase 8: configuration is manageable through the API without losing what
# invariant 8 exists for — one declarative document, outside the database,
# always replaced whole, and never persisted if it would not load.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

API=http://registry:8080/api/v1
admin="$(registry_token "ops-$(date +%s)")"
plain="$(registry_token "ci-plain-$(date +%s)")"

api() { # <method> <path> [curl args...]
  local method="$1" path="$2"; shift 2
  client_curl -sS -X "$method" -H "Authorization: Bearer $admin" "$@" "$API$path"
}

echo "--> the console's status endpoint answers"
status="$(client_curl -sS "$API/status")"
grep -q '"site"' <<<"$status" || { echo "$status" >&2; exit 1; }
grep -q '"config_source"' <<<"$status" || { echo "$status" >&2; exit 1; }

echo "--> whoami distinguishes an administrator from an ordinary identity"
who="$(client_curl -sS -H "Authorization: Bearer $admin" "$API/whoami")"
grep -q '"admin":true' <<<"$who" || { echo "admin not recognised: $who" >&2; exit 1; }
who="$(client_curl -sS -H "Authorization: Bearer $plain" "$API/whoami")"
grep -q '"admin":false' <<<"$who" || { echo "non-admin treated as admin: $who" >&2; exit 1; }

echo "--> a non-administrator cannot read or change the configuration"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' \
  -H "Authorization: Bearer $plain" "$API/config")"
[[ "$code" == "403" ]] || { echo "non-admin read returned $code, want 403" >&2; exit 1; }
code="$(client_curl -sS -o /dev/null -w '%{http_code}' -X PUT \
  -H "Authorization: Bearer $plain" --data '{}' "$API/config/feeds/anything")"
[[ "$code" == "403" ]] || { echo "non-admin write returned $code, want 403" >&2; exit 1; }

echo "--> anonymous callers get 401, not 403 (they may have credentials to offer)"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' "$API/config")"
[[ "$code" == "401" ]] || { echo "anonymous read returned $code, want 401" >&2; exit 1; }

echo "--> the document is served with a version"
version="$(api GET /config -o /dev/null -w '%header{etag}' | tr -d '"')"
[[ ${#version} -eq 64 ]] || { echo "no usable ETag: '$version'" >&2; exit 1; }

echo "--> creating a feed through the API"
NEW_FEED=api-made
api PUT "/config/feeds/$NEW_FEED" -o /dev/null -w '%{http_code}' \
  -H 'Content-Type: application/json' \
  --data "{\"name\":\"$NEW_FEED\",\"format\":\"maven\",\"upstream\":\"http://fake-upstream/maven\",\"anonymous\":true}" \
  | grep -qE '^(200|201)$' || { echo "feed creation failed" >&2; exit 1; }

echo "--> the new feed serves packages without a restart"
serves() {
  local code
  code="$(client_curl -sS -o /dev/null -w '%{http_code}' \
    "http://registry:8080/maven/$NEW_FEED/com/example/liba/1.0.0/liba-1.0.0.jar")"
  [[ "$code" == "200" ]]
}
if ! wait_for_conformance 60 serves; then
  echo "the feed created through the API never started serving" >&2
  exit 1
fi

echo "--> an invalid document is rejected and NOT stored"
before="$(api GET /config -o /dev/null -w '%header{etag}' | tr -d '"')"
code="$(api PUT "/config/feeds/broken" -o /dev/null -w '%{http_code}' \
  -H 'Content-Type: application/json' \
  --data '{"name":"broken","format":"not-a-real-format"}')"
[[ "$code" == "422" ]] || { echo "invalid feed returned $code, want 422" >&2; exit 1; }
after="$(api GET /config -o /dev/null -w '%header{etag}' | tr -d '"')"
[[ "$before" == "$after" ]] || { echo "a rejected document changed the version" >&2; exit 1; }

echo "--> a stale write is refused with 409"
code="$(api PUT "/config/feeds/$NEW_FEED" -o /dev/null -w '%{http_code}' \
  -H 'Content-Type: application/json' \
  -H 'If-Match: "0000000000000000000000000000000000000000000000000000000000000000"' \
  --data "{\"name\":\"$NEW_FEED\",\"format\":\"maven\",\"upstream\":\"http://fake-upstream/maven\"}")"
[[ "$code" == "409" ]] || { echo "stale write returned $code, want 409" >&2; exit 1; }

echo "--> deleting the feed stops it serving, and leaves the packages alone"
api DELETE "/config/feeds/$NEW_FEED" -o /dev/null -w '%{http_code}' | grep -q '^200$' \
  || { echo "feed deletion failed" >&2; exit 1; }
gone() {
  local code
  code="$(client_curl -sS -o /dev/null -w '%{http_code}' \
    "http://registry:8080/maven/$NEW_FEED/com/example/liba/1.0.0/liba-1.0.0.jar")"
  [[ "$code" == "404" ]]
}
if ! wait_for_conformance 60 gone; then
  echo "the deleted feed is still serving" >&2
  exit 1
fi

echo "--> tokens are listed without ever exposing a secret"
tokens="$(api GET /tokens)"
grep -q '"hash_prefix"' <<<"$tokens" || { echo "$tokens" >&2; exit 1; }
grep -q '"secret"' <<<"$tokens" && { echo "the token listing exposed a secret" >&2; exit 1; }

echo "admin api ok"
