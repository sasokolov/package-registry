#!/usr/bin/env bash
# Chaos: stop PostgreSQL. The read path keeps working with a token that is
# already in the auth cache (invariant 7), while publishing answers 503 with
# a clear body instead of hanging or 500-ing.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

restore_pg() {
  compose up -d --wait postgres >/dev/null 2>&1 || true
}
trap restore_pg EXIT

token="$(fondaco_token "ci-chaos-$(date +%s)")"
PRIVATE=http://lb/maven/private/com/example/liba/1.0.0/liba-1.0.0.jar
ANON=http://lb/maven/central/com/example/libb/1.0.0/libb-1.0.0.jar
HOSTED=http://lb/maven/hosted/com/example/chaos/1.0.0/chaos-1.0.0.jar

echo "--> warming caches while the database is up"
client_curl -fsS -o /dev/null -H "Authorization: Bearer $token" "$PRIVATE"
client_curl -fsS -o /dev/null "$ANON"

echo "--> stopping postgres"
compose stop postgres >/dev/null

echo "--> anonymous reads keep working"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' "$ANON")"
if [[ "$code" != "200" ]]; then
  echo "anonymous read returned $code while the database is down" >&2
  exit 1
fi

echo "--> authenticated reads keep working from the cached token"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $token" "$PRIVATE")"
if [[ "$code" != "200" ]]; then
  echo "authenticated read returned $code while the database is down" >&2
  exit 1
fi

echo "--> publishing answers 503 with a readable body"
body="$(client_curl -sS -o - -w '\n%{http_code}' -X PUT --data-binary 'x' \
  -H "Authorization: Bearer $token" "$HOSTED")"
code="$(tail -1 <<<"$body")"
text="$(sed '$d' <<<"$body")"
if [[ "$code" != "503" ]]; then
  echo "publish returned $code while the database is down, want 503" >&2
  echo "$text" >&2
  exit 1
fi
if ! grep -qi 'unavailable' <<<"$text"; then
  echo "503 body is not explanatory: $text" >&2
  exit 1
fi

echo "postgres outage survived"
