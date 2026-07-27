#!/usr/bin/env bash
# Phase 14: access control as named policies with explicit capabilities and
# explicit denies.
#
# Two things have to be true at once. Everything that worked before still
# works — the older anonymous/publishers/admins fields compile into the same
# engine, so nothing about them is a special case any more. And things that
# were not expressible before now are: publishing into one coordinate
# namespace and not another, and a deny that a narrower rule can except.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

BASE=http://registry:8080
API=$BASE/api/v1

admin="$(registry_token "ops-access-$(date +%s)")"
team="$(registry_token "team-example-$(date +%s)")"
guarded="$(registry_token "guarded-reader-$(date +%s)")"
oncall="$(registry_token "oncall-$(date +%s)")"
nobody="$(registry_token "nobody-$(date +%s)")"

publish() { # <token> <feed> <path> <content>
  client_curl -sS -o /dev/null -w '%{http_code}' -X PUT \
    -H "Authorization: Bearer $1" --data-binary "$4" "$BASE/maven/$2/$3"
}

echo "--> a policy can grant publishing inside one coordinate namespace"
code="$(publish "$team" hosted "com/example/policy/1.0.0/policy-1.0.0.jar" "allowed")"
[[ "$code" == "201" ]] || { echo "publishing into the granted namespace returned $code" >&2; exit 1; }

echo "--> and refuse it outside that namespace, with the coordinate in the reason"
body="$(client_curl -sS -w '\n%{http_code}' -X PUT -H "Authorization: Bearer $team" \
  --data-binary "denied" \
  "$BASE/maven/hosted/com/forbidden/thing/1.0.0/thing-1.0.0.jar")"
code="$(tail -n1 <<<"$body")"
[[ "$code" == "403" ]] || {
  echo "publishing outside the namespace returned $code, want 403" >&2; echo "$body" >&2; exit 1; }
grep -q 'denies' <<<"$body" || {
  echo "the refusal does not say a rule denied it: $body" >&2; exit 1; }

echo "--> a coordinate no rule mentions is refused too: the default is no"
body="$(client_curl -sS -w '\n%{http_code}' -X PUT -H "Authorization: Bearer $team" \
  --data-binary "x" "$BASE/maven/hosted/org/other/thing/1.0.0/thing-1.0.0.jar")"
code="$(tail -n1 <<<"$body")"
[[ "$code" == "403" ]] || { echo "an unmentioned coordinate returned $code, want 403" >&2; exit 1; }

echo "--> a narrow grant is an exception to a broad deny"
# Every authenticated identity may read every feed, by the policies compiled
# from the older fields. guarded-reader denies all of private and excepts one
# artifact, so both halves of the deny have something real to act on.
code="$(client_curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $guarded" \
  "$BASE/maven/private/com/example/liba/1.0.0/liba-1.0.0.pom")"
[[ "$code" == "200" ]] || { echo "the excepted artifact returned $code, want 200" >&2; exit 1; }

body="$(client_curl -sS -w '\n%{http_code}' -H "Authorization: Bearer $guarded" \
  "$BASE/maven/private/com/example/libb/1.0.0/libb-1.0.0.pom")"
code="$(tail -n1 <<<"$body")"
[[ "$code" == "403" ]] || { echo "the denied artifact returned $code, want 403" >&2; exit 1; }
grep -q 'denies' <<<"$body" || { echo "the refusal does not name a deny: $body" >&2; exit 1; }

# An identity the deny does not apply to still reads the whole feed.
code="$(client_curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $admin" \
  "$BASE/maven/private/com/example/libb/1.0.0/libb-1.0.0.pom")"
[[ "$code" == "200" ]] || { echo "the deny leaked to another identity ($code)" >&2; exit 1; }

echo "--> capabilities are separable: quarantine without configuration"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $oncall" -H 'Content-Type: application/json' \
  --data '{"feed":"hosted","coordinate":"maven:com.example:policy@1.0.0","detail":"under review"}' \
  "$API/quarantine")"
[[ "$code" == "200" ]] || { echo "the on-call identity could not quarantine: $code" >&2; exit 1; }

code="$(client_curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $oncall" "$API/config")"
[[ "$code" == "403" ]] || {
  echo "the on-call identity could read the configuration ($code); the capability is not separable" >&2
  exit 1; }

# And the block it made is real.
code="$(client_curl -sS -o /dev/null -w '%{http_code}' \
  "$BASE/maven/hosted/com/example/policy/1.0.0/policy-1.0.0.jar")"
[[ "$code" == "409" ]] || { echo "the quarantined coordinate returned $code, want 409" >&2; exit 1; }
client_curl -sS -o /dev/null -X POST -H "Authorization: Bearer $oncall" \
  -H 'Content-Type: application/json' \
  --data '{"feed":"hosted","coordinate":"maven:com.example:policy@1.0.0","active":false}' \
  "$API/quarantine"

echo "--> an identity no binding selects gets nothing beyond the public feeds"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' -X PUT \
  -H "Authorization: Bearer $nobody" --data-binary "x" \
  "$BASE/maven/hosted/com/example/nope/1.0.0/nope-1.0.0.jar")"
[[ "$code" == "403" ]] || { echo "an unbound identity published ($code)" >&2; exit 1; }

echo "--> the older fields still mean what they meant"
# An anonymous feed is still world-readable...
code="$(client_curl -sS -o /dev/null -w '%{http_code}' \
  "$BASE/maven/central/com/example/liba/1.0.0/liba-1.0.0.pom")"
[[ "$code" == "200" ]] || { echo "the anonymous feed returned $code" >&2; exit 1; }
# ...a feed that is not anonymous still needs a credential...
code="$(client_curl -sS -o /dev/null -w '%{http_code}' \
  "$BASE/maven/private/com/example/ok/1.0.0/ok-1.0.0.jar")"
[[ "$code" == "401" ]] || { echo "the private feed returned $code to a stranger, want 401" >&2; exit 1; }
# ...and publishers keep publishing.
ci="$(registry_token "ci-access-$(date +%s)")"
code="$(publish "$ci" hosted "com/example/legacy/1.0.0/legacy-1.0.0.jar" "legacy")"
[[ "$code" == "201" ]] || { echo "a publishers-based identity got $code" >&2; exit 1; }

echo "--> the rules can be read back, generated ones marked as such"
rules="$(client_curl -fsS -H "Authorization: Bearer $admin" "$API/access")"
grep -q '"team-example"' <<<"$rules" || { echo "$rules" >&2; exit 1; }
grep -q '"generated":true' <<<"$rules" || {
  echo "policies compiled from the older fields are not marked:" >&2; echo "$rules" >&2; exit 1; }

echo "--> and a decision can be explained, which is the point of writing it down"
explain="$(client_curl -fsS -H "Authorization: Bearer $team" \
  "$API/access/explain?path=feed/hosted/maven:com.forbidden:x@1.0.0&capability=publish")"
grep -q '"allowed":false' <<<"$explain" || { echo "$explain" >&2; exit 1; }
grep -q '"policy":"team-example"' <<<"$explain" || {
  echo "the explanation does not name the deciding policy:" >&2; echo "$explain" >&2; exit 1; }
grep -q 'maven:com.forbidden' <<<"$explain" || { echo "$explain" >&2; exit 1; }

explain="$(client_curl -fsS -H "Authorization: Bearer $team" \
  "$API/access/explain?path=feed/hosted/maven:com.example:x@1.0.0&capability=publish")"
grep -q '"allowed":true' <<<"$explain" || { echo "$explain" >&2; exit 1; }

echo "--> an administrator can ask what somebody else would be allowed"
explain="$(client_curl -fsS -H "Authorization: Bearer $admin" \
  "$API/access/explain?kind=token&subject=team-future&path=feed/hosted/maven:com.example:x&capability=publish")"
grep -q '"allowed":true' <<<"$explain" || {
  echo "asking about another identity did not work:" >&2; echo "$explain" >&2; exit 1; }
# But an ordinary identity may not.
code="$(client_curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $team" \
  "$API/access/explain?kind=token&subject=ops-someone&path=sys/config&capability=update")"
[[ "$code" == "403" ]] || {
  echo "an ordinary identity could ask about someone else ($code)" >&2; exit 1; }

echo "OK: access policies"
