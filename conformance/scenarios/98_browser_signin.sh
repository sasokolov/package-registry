#!/usr/bin/env bash
# Signing in to the console through an identity provider.
#
# The unit tests drive the exchange against a fake issuer in-process; this
# drives the whole thing the way a person does, through a real provider in a
# container: the site advertises a button, the registry builds a redirect from
# its own configuration, the provider approves, and what comes back is an
# ordinary credential that the ordinary request path accepts.
#
# The failures matter as much as the success. A public client's only
# protection is PKCE, so a code without its verifier, a code used twice and an
# issuer nobody configured all have to be refused here, not merely refused in
# a unit test.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

BASE=http://registry:8080
API=$BASE/api/v1
ISSUER=http://fake-oidc:9099

# base64url of a sha256 digest, which is all PKCE's S256 method is.
challenge_of() {
  printf '%s' "$1" | openssl dgst -sha256 -binary | openssl base64 |
    tr '+/' '-_' | tr -d '=\n'
}

# start_flow prints the authorization URL for a fresh sign-in.
start_flow() { # <state> <nonce> <challenge>
  # Go's JSON encoder escapes "&" as \u0026; a browser's JSON.parse undoes
  # that, and so must a shell that wants to follow the URL.
  client_curl -fsS "$API/auth/oidc/authorize?issuer=$ISSUER&state=$1&nonce=$2&code_challenge=$3" |
    sed -n 's/.*"authorization_url":"\([^"]*\)".*/\1/p' |
    sed 's/\\u0026/\&/g'
}

# approve follows the redirect a person would see and prints the code.
approve() { # <authorization-url>
  client_curl -sS -o /dev/null -D- "$1&approve=yes" |
    tr -d '\r' | sed -n 's|^[Ll]ocation:.*[?&]code=\([^&]*\).*|\1|p'
}

exchange() { # <code> <verifier> <nonce>
  client_curl -sS -w '\n%{http_code}' -X POST -H 'Content-Type: application/json' \
    --data "{\"issuer\":\"$ISSUER\",\"code\":\"$1\",\"code_verifier\":\"$2\",\"nonce\":\"$3\"}" \
    "$API/auth/oidc/exchange"
}

echo "--> the site advertises a sign-in button, not a field to paste into"
methods="$(client_curl -fsS "$API/auth/methods")"
grep -q "\"issuer\":\"$ISSUER\"" <<<"$methods" || {
  echo "the configured issuer is not advertised:" >&2; echo "$methods" >&2; exit 1; }
grep -q '"flow":"browser"' <<<"$methods" || {
  echo "an issuer with a client_id is not offered as a browser sign-in:" >&2
  echo "$methods" >&2; exit 1; }
# A static token is still a static token: adding a button must not have
# turned every method into one.
grep -q '"type":"token","label":"Registry token"' <<<"$methods" || {
  echo "$methods" >&2; exit 1; }

echo "--> the redirect is built from the registry's configuration"
verifier="conformance-verifier-$(date +%s)-0123456789012345"
challenge="$(challenge_of "$verifier")"
state="state-$(date +%s)"
nonce="nonce-$(date +%s)"

url="$(start_flow "$state" "$nonce" "$challenge")"
[[ -n "$url" ]] || { echo "no authorization_url came back" >&2; exit 1; }
for expected in \
  "$ISSUER/authorize" \
  "client_id=registry-console" \
  "code_challenge=$challenge" \
  "code_challenge_method=S256" \
  "response_type=code" \
  "state=$state" \
  "nonce=$nonce"
do
  grep -qF "$expected" <<<"$url" || {
    echo "the authorization URL is missing $expected:" >&2; echo "$url" >&2; exit 1; }
done
# The endpoint was discovered, not configured: the issuer's document is what
# said /authorize, and following it is the part that breaks silently.
grep -qF "redirect_uri=http%3A%2F%2Fregistry%3A8080%2Fui%2Foidc%2Fcallback" <<<"$url" || {
  echo "the redirect URI is not this site's console callback:" >&2; echo "$url" >&2; exit 1; }

echo "--> the provider approves and sends the browser back with a code"
code="$(approve "$url")"
[[ -n "$code" ]] || { echo "the provider redirected without a code" >&2; exit 1; }

echo "--> and the code becomes a credential this registry accepts"
body="$(exchange "$code" "$verifier" "$nonce")"
status="$(tail -n1 <<<"$body")"
[[ "$status" == "200" ]] || { echo "exchange returned $status" >&2; echo "$body" >&2; exit 1; }
id_token="$(sed -n 's/.*"id_token":"\([^"]*\)".*/\1/p' <<<"$body")"
[[ -n "$id_token" ]] || { echo "no id_token in $body" >&2; exit 1; }
grep -q '"expires_at"' <<<"$body" || {
  echo "the console is not told when the credential runs out:" >&2; echo "$body" >&2; exit 1; }

whoami="$(client_curl -fsS -H "Authorization: Bearer $id_token" "$API/whoami")"
grep -q '"kind":"oidc"' <<<"$whoami" || { echo "$whoami" >&2; exit 1; }
grep -q '"subject":"alice@example.com"' <<<"$whoami" || { echo "$whoami" >&2; exit 1; }

# It is an ordinary credential: the serving path takes it too, which is the
# whole reason the console has no session of its own. The private feed
# refuses a stranger outright, so anything but a 401 means the credential was
# accepted — whether the artifact happens to exist is another question.
artifact="$BASE/maven/private/com/example/signin/1.0.0/signin-1.0.0.jar"
anonymous_status="$(client_curl -sS -o /dev/null -w '%{http_code}' "$artifact")"
[[ "$anonymous_status" == "401" ]] || {
  echo "the private feed answered a stranger with $anonymous_status; the check below proves nothing" >&2
  exit 1; }
signed_in_status="$(client_curl -sS -o /dev/null -w '%{http_code}' \
  -H "Authorization: Bearer $id_token" "$artifact")"
[[ "$signed_in_status" != "401" ]] || {
  echo "a browser sign-in's credential was refused on the serving path" >&2; exit 1; }

echo "--> a code cannot be spent twice"
body="$(exchange "$code" "$verifier" "$nonce")"
status="$(tail -n1 <<<"$body")"
[[ "$status" == "401" ]] || { echo "replaying a code returned $status, want 401" >&2; exit 1; }

echo "--> and is worthless to anyone who did not start the flow"
verifier2="conformance-verifier-two-$(date +%s)-01234567890"
url="$(start_flow "s2-$state" "n2-$nonce" "$(challenge_of "$verifier2")")"
code="$(approve "$url")"
body="$(exchange "$code" "a-guess-that-is-long-enough-0123456789" "n2-$nonce")"
status="$(tail -n1 <<<"$body")"
[[ "$status" == "401" ]] || {
  echo "a code was redeemed without its verifier ($status)" >&2; echo "$body" >&2; exit 1; }

echo "--> a token minted for another sign-in is not this sign-in's answer"
verifier3="conformance-verifier-three-$(date +%s)-0123456789"
url="$(start_flow "s3-$state" "the-nonce-the-issuer-saw" "$(challenge_of "$verifier3")")"
code="$(approve "$url")"
body="$(exchange "$code" "$verifier3" "a-different-nonce")"
status="$(tail -n1 <<<"$body")"
[[ "$status" == "401" ]] || { echo "a nonce mismatch was accepted ($status)" >&2; exit 1; }

echo "--> the registry only ever talks to issuers it was configured with"
body="$(client_curl -sS -w '\n%{http_code}' -X POST -H 'Content-Type: application/json' \
  --data '{"issuer":"http://attacker.example","code":"x","code_verifier":"y"}' \
  "$API/auth/oidc/exchange")"
status="$(tail -n1 <<<"$body")"
[[ "$status" == "401" ]] || {
  echo "an unconfigured issuer was accepted ($status)" >&2; echo "$body" >&2; exit 1; }

code_status="$(client_curl -sS -o /dev/null -w '%{http_code}' \
  "$API/auth/oidc/authorize?issuer=http://attacker.example&state=s&nonce=n&code_challenge=c")"
[[ "$code_status" == "401" ]] || {
  echo "the registry offered to redirect to an unconfigured issuer ($code_status)" >&2; exit 1; }

echo "--> starting a flow needs the parts only a browser can supply"
code_status="$(client_curl -sS -o /dev/null -w '%{http_code}' \
  "$API/auth/oidc/authorize?issuer=$ISSUER")"
[[ "$code_status" == "400" ]] || {
  echo "a flow started with no challenge, state or nonce ($code_status)" >&2; exit 1; }

echo "--> and none of the flow's secrets reach the log"
logs="$(compose logs registry 2>/dev/null | tail -n 400)"
if grep -qF "$verifier" <<<"$logs"; then
  echo "a code_verifier was logged" >&2; exit 1
fi
if grep -qF "$id_token" <<<"$logs"; then
  echo "an id_token was logged" >&2; exit 1
fi
grep -q "browser sign-in" <<<"$logs" || {
  echo "the sign-in was not audited at all" >&2; exit 1; }

echo "OK: browser sign-in"
