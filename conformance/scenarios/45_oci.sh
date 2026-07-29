#!/usr/bin/env bash
# Container images, driven by a real OCI client against a real registry
# upstream: proxying one, hosting one, and a group over both.
#
# This protocol is the one that cannot be faked with a directory of files.
# An image is a manifest naming blobs, each addressed by its own digest, and
# a push is a conversation whose every answer says where the next request
# goes — so the upstream here is an actual registry:2 and the client is
# crane, which speaks exactly what `docker push` speaks without needing a
# daemon inside the compose network. (`docker pull` and `docker push` against
# this registry are exercised on the dev stand, where a daemon exists.)
#
# What is checked here is what silently breaks otherwise: that a digest
# survives proxying byte for byte, that a tag can move while the image it
# used to name stays exactly where it was, and that a group shows the tags of
# every member rather than the first member's.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

BASE=http://registry:8080
UPSTREAM=oci-upstream:5000
PROXY=registry:8080/oci/images-upstream
HOSTED=registry:8080/oci/oci-hosted
GROUP=registry:8080/oci/oci-public

token="$(fondaco_token "ci-oci-$(date +%s)")"

# Every invocation is a fresh container, so a step that depends on a previous
# one — a login, a pulled image — has to run inside a single shell.
crane_sh() { compose run --rm -T --entrypoint /busybox/sh crane-client -c "$1" 2>&1; }

echo "--> seed the upstream registry with a real image"
out="$(crane_sh "
  set -e
  mkdir -p /tmp/layer/etc
  echo conformance > /tmp/layer/etc/demo.txt
  tar -cf /tmp/layer.tar -C /tmp/layer .
  crane append --oci-empty-base -f /tmp/layer.tar -t $UPSTREAM/demo/app:1.0.0 --insecure
  crane digest --insecure $UPSTREAM/demo/app:1.0.0
")" || { echo "$out" >&2; exit 1; }
upstream_digest="$(tail -n1 <<<"$out")"
[[ "$upstream_digest" == sha256:* ]] || { echo "$out" >&2; exit 1; }

echo "--> the proxy serves the same image, byte for byte"
out="$(crane_sh "crane digest --insecure $PROXY/demo/app:1.0.0")" || { echo "$out" >&2; exit 1; }
proxied_digest="$(tail -n1 <<<"$out")"
[[ "$proxied_digest" == "$upstream_digest" ]] || {
  echo "the proxied manifest has digest $proxied_digest, upstream says $upstream_digest" >&2
  echo "a rewritten manifest is a different image, and every client would refuse it" >&2
  exit 1; }

# The digest a client reads out of the response, rather than one it computed.
headers="$(client_curl -sS -o /dev/null -D - \
  -H "Accept: application/vnd.oci.image.manifest.v1+json" \
  "$BASE/v2/oci/images-upstream/demo/app/manifests/1.0.0" | tr -d '\r')"
grep -qi "^docker-content-digest: $upstream_digest" <<<"$headers" || {
  echo "$headers" >&2; exit 1; }
grep -qi "^content-type: application/vnd.oci.image.manifest.v1" <<<"$headers" || {
  echo "the media type did not survive the cache; a client dispatches on it:" >&2
  echo "$headers" >&2; exit 1; }

echo "--> a whole image pulls through the proxy, and then from the cache"
out="$(crane_sh "
  set -e
  crane pull --insecure $PROXY/demo/app:1.0.0 /tmp/pulled.tar
  ls -l /tmp/pulled.tar
")" || { echo "$out" >&2; exit 1; }

source_header="$(client_curl -sS -o /dev/null -D - \
  -H "Accept: application/vnd.oci.image.manifest.v1+json" \
  "$BASE/v2/oci/images-upstream/demo/app/manifests/1.0.0" | tr -d '\r' |
  grep -i '^x-registry-source' | awk '{print $2}')"
[[ "$source_header" == "cache" ]] || {
  echo "the second manifest fetch came from $source_header, want cache" >&2; exit 1; }

echo "--> an image pushes to the hosted feed and comes back identical"
out="$(crane_sh "
  set -e
  crane auth login registry:8080 -u ci -p $token
  crane copy --insecure $UPSTREAM/demo/app:1.0.0 $HOSTED/demo/app:1.0.0
  crane digest --insecure $HOSTED/demo/app:1.0.0
")" || { echo "$out" >&2; exit 1; }
hosted_digest="$(tail -n1 <<<"$out")"
[[ "$hosted_digest" == "$upstream_digest" ]] || {
  echo "the hosted copy is $hosted_digest, the original is $upstream_digest" >&2; exit 1; }

echo "--> the hosted feed lists what it holds"
tags="$(client_curl -fsS "$BASE/v2/oci/oci-hosted/demo/app/tags/list")"
grep -q '"1.0.0"' <<<"$tags" || { echo "$tags" >&2; exit 1; }
catalog="$(client_curl -fsS "$BASE/v2/oci/oci-hosted/_catalog")"
grep -q '"demo/app"' <<<"$catalog" || { echo "$catalog" >&2; exit 1; }

echo "--> a tag moves; the image it used to name does not"
out="$(crane_sh "
  set -e
  crane auth login registry:8080 -u ci -p $token
  mkdir -p /tmp/layer2/etc
  echo second > /tmp/layer2/etc/demo.txt
  tar -cf /tmp/layer2.tar -C /tmp/layer2 .
  crane append --oci-empty-base -f /tmp/layer2.tar -t $UPSTREAM/demo/app:2.0.0 --insecure
  crane copy --insecure $UPSTREAM/demo/app:1.0.0 $HOSTED/demo/app:latest
  crane copy --insecure $UPSTREAM/demo/app:2.0.0 $HOSTED/demo/app:latest
  crane digest --insecure $HOSTED/demo/app:latest
")" || { echo "$out" >&2; exit 1; }
moved="$(tail -n1 <<<"$out")"
[[ "$moved" != "$upstream_digest" ]] || {
  echo "the tag did not move: a tag that cannot be repointed is not a tag" >&2; exit 1; }

code="$(client_curl -sS -o /dev/null -w '%{http_code}' \
  -H "Accept: application/vnd.oci.image.manifest.v1+json" \
  "$BASE/v2/oci/oci-hosted/demo/app/manifests/$upstream_digest")"
[[ "$code" == "200" ]] || {
  echo "the image the tag used to point at answers $code (invariant 4)" >&2; exit 1; }

echo "--> a manifest referencing bytes nobody uploaded is refused"
missing="sha256:$(printf 'nothing was uploaded' | sha256sum | awk '{print $1}')"
body="$(client_curl -sS -w '\n%{http_code}' -X PUT -H "Authorization: Bearer $token" \
  -H "Content-Type: application/vnd.oci.image.manifest.v1+json" \
  --data-binary "{\"schemaVersion\":2,\"mediaType\":\"application/vnd.oci.image.manifest.v1+json\",\"config\":{\"mediaType\":\"application/vnd.oci.image.config.v1+json\",\"digest\":\"$missing\",\"size\":1},\"layers\":[]}" \
  "$BASE/v2/oci/oci-hosted/demo/broken/manifests/1.0")"
code="$(tail -n1 <<<"$body")"
[[ "$code" == "400" ]] || {
  echo "an image that cannot be pulled was published anyway ($code)" >&2; echo "$body" >&2; exit 1; }

echo "--> deleting a published image is refused, with somewhere to go"
body="$(client_curl -sS -w '\n%{http_code}' -X DELETE -H "Authorization: Bearer $token" \
  "$BASE/v2/oci/oci-hosted/demo/app/manifests/$upstream_digest")"
code="$(tail -n1 <<<"$body")"
[[ "$code" == "403" ]] || { echo "delete returned $code, want 403" >&2; echo "$body" >&2; exit 1; }
grep -qi "quarantine" <<<"$body" || {
  echo "the refusal does not say what to do instead:" >&2; echo "$body" >&2; exit 1; }

echo "--> the group shows the hosted image AND the proxied one"
out="$(crane_sh "
  set -e
  crane ls --insecure $GROUP/demo/app
  crane pull --insecure $GROUP/demo/app:2.0.0 /tmp/group.tar
  ls /tmp/group.tar
")" || { echo "$out" >&2; exit 1; }
grep -q "1.0.0" <<<"$out" || { echo "$out" >&2; exit 1; }
grep -q "latest" <<<"$out" || {
  echo "the hosted tag is missing from the group:" >&2; echo "$out" >&2; exit 1; }
headers="$(client_curl -sS -o /dev/null -D - "$BASE/v2/oci/oci-public/demo/app/tags/list" | tr -d '\r')"
grep -qi '^x-registry-merged: oci-hosted,images-upstream' <<<"$headers" || { echo "$headers" >&2; exit 1; }

echo "--> pushing without the right is refused"
nobody="$(fondaco_token "nobody-oci-$(date +%s)")"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $nobody" \
  "$BASE/v2/oci/oci-hosted/demo/app/blobs/uploads/")"
[[ "$code" == "403" ]] || { echo "an identity without publish rights got $code" >&2; exit 1; }

# An anonymous push must be refused in the way a client can act on: the
# challenge is what tells docker to send credentials at all.
headers="$(client_curl -sS -o /dev/null -D - -X POST \
  "$BASE/v2/oci/oci-hosted/demo/app/blobs/uploads/" | tr -d '\r')"
grep -qi '^www-authenticate: Basic realm=' <<<"$headers" || {
  echo "an anonymous push was refused without a challenge a client can use:" >&2
  echo "$headers" >&2; exit 1; }

echo "OK: oci"
