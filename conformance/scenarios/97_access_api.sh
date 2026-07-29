#!/usr/bin/env bash
# Phase 14: managing access through the API, and advertising how to sign in.
#
# Scenario 96 proves the engine decides correctly from a document written by
# hand. This one proves the other half: that a policy written through the API
# reaches the engine that answers real requests, that removing it takes the
# access away again, and that neither can be done by somebody who is not an
# administrator. An access system whose API and whose enforcement disagree is
# worse than one with no API at all.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

BASE=http://registry:8080
API=$BASE/api/v1

stamp="$(date +%s)"
admin="$(fondaco_token "ops-api-$stamp")"
subject="api-reader-$stamp"
reader="$(fondaco_token "$subject")"
policy="api-reader-$stamp"
binding="api-reader-$stamp"

cleanup() {
  client_curl -sS -o /dev/null -X DELETE -H "Authorization: Bearer $admin" \
    "$API/config/access/bindings/$binding" || true
  client_curl -sS -o /dev/null -X DELETE -H "Authorization: Bearer $admin" \
    "$API/config/access/policies/$policy" || true
}
trap cleanup EXIT

echo "--> the site says how it can be signed in to, before anybody has signed in"
methods="$(client_curl -fsS "$API/auth/methods")"
grep -q '"type":"token"' <<<"$methods" || {
  echo "a site with a database does not advertise tokens:" >&2; echo "$methods" >&2; exit 1; }
grep -q '"label":' <<<"$methods" || {
  echo "the method has nothing to label the field with:" >&2; echo "$methods" >&2; exit 1; }

echo "--> before any policy, the identity is refused"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' -X PUT \
  -H "Authorization: Bearer $reader" --data-binary "x" \
  "$BASE/maven/hosted/com/api/thing/1.0.0/thing-1.0.0.jar")"
[[ "$code" == "403" ]] || { echo "publishing before any grant returned $code, want 403" >&2; exit 1; }

echo "--> an administrator writes a policy and a binding"
created="$(client_curl -fsS -X PUT -H "Authorization: Bearer $admin" \
  -H 'Content-Type: application/json' \
  --data "{\"name\":\"$policy\",\"rules\":[
    {\"path\":\"feed/hosted/maven:com.api:*\",\"capabilities\":[\"read\",\"publish\"]}]}" \
  "$API/config/access/policies/$policy")"
grep -q '"created":true' <<<"$created" || { echo "$created" >&2; exit 1; }
grep -q '"version":' <<<"$created" || {
  echo "the write did not report the version it produced:" >&2; echo "$created" >&2; exit 1; }

created="$(client_curl -fsS -X PUT -H "Authorization: Bearer $admin" \
  -H 'Content-Type: application/json' \
  --data "{\"name\":\"$binding\",\"policies\":[\"$policy\"],
           \"match\":{\"kind\":\"token\",\"subject\":\"$subject\"}}" \
  "$API/config/access/bindings/$binding")"
grep -q '"created":true' <<<"$created" || { echo "$created" >&2; exit 1; }

echo "--> and the grant is in force, not merely stored"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' -X PUT \
  -H "Authorization: Bearer $reader" --data-binary "published by policy" \
  "$BASE/maven/hosted/com/api/thing/1.0.0/thing-1.0.0.jar")"
[[ "$code" == "201" ]] || {
  echo "publishing after the grant returned $code; the API and the engine disagree" >&2; exit 1; }

echo "--> the engine's own explanation names the policy that was just written"
explain="$(client_curl -fsS -H "Authorization: Bearer $reader" \
  "$API/access/explain?path=feed/hosted/maven:com.api:thing@1.0.0&capability=publish")"
grep -q "\"policy\":\"$policy\"" <<<"$explain" || { echo "$explain" >&2; exit 1; }

echo "--> it reads back, and is not confused with the generated ones"
list="$(client_curl -fsS -H "Authorization: Bearer $admin" "$API/config/access/policies")"
grep -q "\"name\":\"$policy\"" <<<"$list" || { echo "$list" >&2; exit 1; }
if grep -q '"name":"sys:admin"' <<<"$list"; then
  echo "generated policies are offered for editing:" >&2; echo "$list" >&2; exit 1
fi
one="$(client_curl -fsS -H "Authorization: Bearer $admin" "$API/config/access/policies/$policy")"
grep -q 'feed/hosted/maven:com.api:\*' <<<"$one" || { echo "$one" >&2; exit 1; }

echo "--> writing it again narrows it in place rather than creating a second one"
updated="$(client_curl -fsS -X PUT -H "Authorization: Bearer $admin" \
  -H 'Content-Type: application/json' \
  --data "{\"name\":\"$policy\",\"rules\":[
    {\"path\":\"feed/hosted/maven:com.api:*\",\"capabilities\":[\"read\"]}]}" \
  "$API/config/access/policies/$policy")"
grep -q '"created":false' <<<"$updated" || { echo "$updated" >&2; exit 1; }
code="$(client_curl -sS -o /dev/null -w '%{http_code}' -X PUT \
  -H "Authorization: Bearer $reader" --data-binary "x" \
  "$BASE/maven/hosted/com/api/other/1.0.0/other-1.0.0.jar")"
[[ "$code" == "403" ]] || { echo "publish survived the narrowing ($code)" >&2; exit 1; }

echo "--> a policy a binding still names cannot be deleted out from under it"
body="$(client_curl -sS -w '\n%{http_code}' -X DELETE -H "Authorization: Bearer $admin" \
  "$API/config/access/policies/$policy")"
code="$(tail -n1 <<<"$body")"
[[ "$code" == "409" ]] || { echo "deleting a bound policy returned $code, want 409" >&2; exit 1; }
grep -q "$binding" <<<"$body" || {
  echo "the refusal does not say which binding to fix:" >&2; echo "$body" >&2; exit 1; }

echo "--> removing the binding takes the access away"
client_curl -fsS -o /dev/null -X DELETE -H "Authorization: Bearer $admin" \
  "$API/config/access/bindings/$binding"
explain="$(client_curl -fsS -H "Authorization: Bearer $reader" \
  "$API/access/explain?path=feed/hosted/maven:com.api:thing@1.0.0&capability=publish")"
grep -q '"allowed":false' <<<"$explain" || {
  echo "the grant outlived its binding:" >&2; echo "$explain" >&2; exit 1; }

echo "--> and then the policy goes"
client_curl -fsS -o /dev/null -X DELETE -H "Authorization: Bearer $admin" \
  "$API/config/access/policies/$policy"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $admin" \
  "$API/config/access/policies/$policy")"
[[ "$code" == "404" ]] || { echo "the deleted policy is still there ($code)" >&2; exit 1; }

echo "--> deleting what is not there is a 404, not a silent success"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' -X DELETE -H "Authorization: Bearer $admin" \
  "$API/config/access/policies/never-existed-$stamp")"
[[ "$code" == "404" ]] || { echo "deleting a missing policy returned $code" >&2; exit 1; }

echo "--> and none of this is available to an identity that is not an administrator"
for target in "policies/$policy" "bindings/$binding"; do
  code="$(client_curl -sS -o /dev/null -w '%{http_code}' -X PUT \
    -H "Authorization: Bearer $reader" -H 'Content-Type: application/json' \
    --data '{"name":"smuggled","rules":[{"path":"sys/config","capabilities":["update"]}]}' \
    "$API/config/access/$target")"
  [[ "$code" == "403" ]] || { echo "a non-administrator wrote $target ($code)" >&2; exit 1; }
done
code="$(client_curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $reader" \
  "$API/config/access/policies")"
[[ "$code" == "403" ]] || { echo "a non-administrator listed the policies ($code)" >&2; exit 1; }

echo "--> an invalid policy is refused whole, leaving the document as it was"
before="$(client_curl -fsS -H "Authorization: Bearer $admin" "$API/config/access/policies")"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' -X PUT \
  -H "Authorization: Bearer $admin" -H 'Content-Type: application/json' \
  --data '{"name":"broken","rules":[{"path":"nowhere/*","capabilities":["read"]}]}' \
  "$API/config/access/policies/broken")"
[[ "$code" == "422" ]] || { echo "a path in no namespace was accepted ($code)" >&2; exit 1; }
after="$(client_curl -fsS -H "Authorization: Bearer $admin" "$API/config/access/policies")"
[[ "$before" == "$after" ]] || {
  echo "a refused write changed the document" >&2; echo "$before" >&2; echo "$after" >&2; exit 1; }

echo "OK: access API"
