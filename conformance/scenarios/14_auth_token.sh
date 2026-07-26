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
secret="$(registry_token "conformance-$(date +%s)")"
if [[ "$secret" != reg_* ]]; then
  echo "token create did not return a secret (got: ${secret:0:8}...)" >&2
  exit 1
fi

code="$(client_curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $secret" "$URL")"
if [[ "$code" != "200" ]]; then
  echo "authenticated request returned $code, want 200" >&2
  exit 1
fi

echo "--> HTTP Basic with the token as password (maven settings.xml form)"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' -u "ci:$secret" "$URL")"
if [[ "$code" != "200" ]]; then
  echo "Basic-authenticated request returned $code, want 200" >&2
  exit 1
fi
if ! grep -qi 'basic' <<<"$www"; then
  echo "401 challenge does not advertise Basic: $www" >&2
  exit 1
fi

echo "--> real mvn resolve against the private feed using settings.xml credentials"
out="$(compose run --rm -T -e REGISTRY_TOKEN="$secret" maven-client \
  -B -s /work/settings-auth.xml -f /work/pom.xml \
  org.apache.maven.plugins:maven-dependency-plugin:3.6.1:resolve 2>&1)" || {
  echo "$out" | tail -30
  exit 1
}
grep -q "BUILD SUCCESS" <<<"$out" || { echo "authenticated mvn resolve failed" >&2; exit 1; }

echo "auth token ok"
