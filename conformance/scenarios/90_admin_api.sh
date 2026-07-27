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
status="$(api GET /status)"
grep -q '"site"' <<<"$status" || { echo "$status" >&2; exit 1; }
grep -q '"config_source"' <<<"$status" || { echo "$status" >&2; exit 1; }

echo "--> a stranger is told the site name and nothing about the deployment"
status="$(client_curl -sS "$API/status")"
grep -q '"site"' <<<"$status" || { echo "$status" >&2; exit 1; }
for leak in config_version config_source database; do
  if grep -q "\"$leak\"" <<<"$status"; then
    echo "anonymous status leaked $leak: $status" >&2; exit 1
  fi
done

echo "--> a stranger sees only the feeds it may read, without their configuration"
feeds="$(client_curl -sS "$API/feeds")"
grep -q '"central"' <<<"$feeds" || { echo "$feeds" >&2; exit 1; }
if grep -q '"private"' <<<"$feeds"; then
  echo "anonymous feed listing revealed a feed that needs authentication: $feeds" >&2; exit 1
fi
for leak in publishers upstream policies packages; do
  if grep -q "\"$leak\"" <<<"$feeds"; then
    echo "anonymous feed listing leaked $leak: $feeds" >&2; exit 1
  fi
done
# An identified caller does get it.
feeds="$(client_curl -sS -H "Authorization: Bearer $plain" "$API/feeds")"
grep -q '"private"' <<<"$feeds" || { echo "an identified caller cannot see the feeds: $feeds" >&2; exit 1; }
grep -q '"upstream"' <<<"$feeds" || { echo "$feeds" >&2; exit 1; }

echo "--> a refused credential is reported as refused, not as anonymity"
who="$(client_curl -sS -H "Authorization: Bearer reg_not-a-real-token" "$API/whoami")"
grep -q '"kind":"anonymous"' <<<"$who" || { echo "$who" >&2; exit 1; }
grep -q '"auth_error"' <<<"$who" || {
  echo "a rejected token was reported as plain anonymity: $who" >&2; exit 1; }
# Offering nothing is not an error.
who="$(client_curl -sS "$API/whoami")"
if grep -q '"auth_error"' <<<"$who"; then
  echo "browsing anonymously was reported as an authentication failure: $who" >&2; exit 1
fi

echo "--> the operational endpoints need an identity"
for endpoint in /replication /conflicts /quarantine; do
  code="$(client_curl -sS -o /dev/null -w '%{http_code}' "$API$endpoint")"
  [[ "$code" == "401" ]] || { echo "anonymous $endpoint returned $code, want 401" >&2; exit 1; }
  code="$(client_curl -sS -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer $plain" "$API$endpoint")"
  [[ "$code" == "200" ]] || { echo "identified $endpoint returned $code, want 200" >&2; exit 1; }
done

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

echo "--> one administrator binding can be added without rewriting the list"
before_admins="$(api GET /config/admins)"
grep -q '"token:ops-\*"' <<<"$before_admins" || { echo "$before_admins" >&2; exit 1; }
api PUT '/config/admins/binding?pattern=project%3Atf-conf%2F*' -o /dev/null -w '%{http_code}' \
  | grep -qE '^(200|201)$' || { echo "adding a binding failed" >&2; exit 1; }
after_admins="$(api GET /config/admins)"
grep -q '"project:tf-conf/\*"' <<<"$after_admins" || { echo "$after_admins" >&2; exit 1; }
# The administrator this scenario is authenticating as must still be there,
# or the next request would be a 403 caused by the previous one.
grep -q '"token:ops-\*"' <<<"$after_admins" || { echo "the existing administrator was dropped: $after_admins" >&2; exit 1; }
api DELETE '/config/admins/binding?pattern=project%3Atf-conf%2F*' -o /dev/null -w '%{http_code}' \
  | grep -q '^200$' || { echo "removing a binding failed" >&2; exit 1; }

echo "--> the last administrator cannot be removed through the API"
code="$(api DELETE '/config/admins/binding?pattern=token%3Aops-*' -o /dev/null -w '%{http_code}')"
[[ "$code" == "400" ]] || { echo "removing the last administrator returned $code, want 400" >&2; exit 1; }

echo "--> an OIDC issuer is addressed by its URL, not by a mangled path"
api PUT /config/oidc/issuer -o /dev/null -w '%{http_code}' \
  -H 'Content-Type: application/json' \
  --data '{"issuer":"https://gitlab.conf.example.com","audience":"registry"}' \
  | grep -qE '^(200|201)$' || { echo "adding an issuer failed" >&2; exit 1; }
issuer="$(api GET '/config/oidc/issuer?issuer=https%3A%2F%2Fgitlab.conf.example.com')"
grep -q '"audience":"registry"' <<<"$issuer" || { echo "$issuer" >&2; exit 1; }
api DELETE '/config/oidc/issuer?issuer=https%3A%2F%2Fgitlab.conf.example.com' -o /dev/null -w '%{http_code}' \
  | grep -q '^200$' || { echo "removing an issuer failed" >&2; exit 1; }

echo "--> a feed round-trips through the API with every field intact"
api PUT /config/feeds/roundtrip -o /dev/null -w '%{http_code}' \
  -H 'Content-Type: application/json' \
  --data '{"name":"roundtrip","format":"maven","hosted":true,"anonymous":true,
           "publishers":["token:ci-*"],"redirect_ttl":"20m","upstream_rps":5,
           "replication_mode":"eager","peer_fallback":true,
           "policies":[{"name":"allowlist","config":{"allow":["com.example:liba"]}}]}' \
  | grep -qE '^(200|201)$' || { echo "writing the feed failed" >&2; exit 1; }
roundtrip="$(api GET /config/feeds/roundtrip)"
for want in '"replication_mode":"eager"' '"peer_fallback":true' '"redirect_ttl":"20m0s"' \
            '"upstream_rps":5' '"publishers":["token:ci-*"]' '"name":"allowlist"'; do
  # -F: these are JSON fragments, and "[token:ci-*]" is a character range
  # to a regex engine.
  grep -qF -- "$want" <<<"$roundtrip" || {
    echo "the feed lost $want on the way through the API: $roundtrip" >&2; exit 1; }
done

echo "--> a misspelt field is refused, not silently dropped"
code="$(api PUT /config/feeds/roundtrip -o /dev/null -w '%{http_code}' \
  -H 'Content-Type: application/json' \
  --data '{"name":"roundtrip","format":"maven","hosted":true,"anonymus":true}')"
[[ "$code" == "400" ]] || { echo "a misspelt field returned $code, want 400" >&2; exit 1; }
api DELETE /config/feeds/roundtrip -o /dev/null -w '%{http_code}' | grep -q '^200$' \
  || { echo "cleanup failed" >&2; exit 1; }

echo "--> tokens are listed without ever exposing a secret"
tokens="$(api GET /tokens)"
grep -q '"hash_prefix"' <<<"$tokens" || { echo "$tokens" >&2; exit 1; }
grep -q '"secret"' <<<"$tokens" && { echo "the token listing exposed a secret" >&2; exit 1; }

echo "admin api ok"
