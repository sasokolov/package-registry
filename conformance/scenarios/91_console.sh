#!/usr/bin/env bash
# Phase 9: the console is served by the registry itself — at the site root,
# as a single-page application, with caching rules that match what each file
# actually is, and without taking a single path away from the feeds.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

BASE=http://registry:8080

echo "--> the site root points at the console"
headers="$(client_curl -sS -o /dev/null -D - "$BASE/")"
grep -qiE '^HTTP/[0-9.]+ 302' <<<"$headers" || { echo "$headers" >&2; exit 1; }
grep -qi '^location: /ui/' <<<"$headers" || { echo "$headers" >&2; exit 1; }

echo "--> the console is built into the binary and served from it"
index="$(client_curl -sS "$BASE/ui/")"
grep -q 'id="root"' <<<"$index" || { echo "no application root in the document: $index" >&2; exit 1; }

echo "--> the document is revalidated, carries a content policy and no sniffing"
headers="$(client_curl -sS -o /dev/null -D - "$BASE/ui/")"
grep -qi '^cache-control: no-cache' <<<"$headers" || { echo "$headers" >&2; exit 1; }
grep -qi "^content-security-policy: .*default-src 'self'" <<<"$headers" || { echo "$headers" >&2; exit 1; }
grep -qi '^x-content-type-options: nosniff' <<<"$headers" || { echo "$headers" >&2; exit 1; }
grep -qi '^content-type: text/html' <<<"$headers" || { echo "$headers" >&2; exit 1; }

echo "--> a deep link is a client route, not a 404"
for route in /ui/feeds /ui/feeds/central /ui/replication /ui/config; do
  body="$(client_curl -sS -w '\n%{http_code}' "$BASE$route")"
  code="$(tail -n1 <<<"$body")"
  [[ "$code" == "200" ]] || { echo "$route returned $code, want 200" >&2; exit 1; }
  grep -q 'id="root"' <<<"$body" || { echo "$route did not serve the document" >&2; exit 1; }
done

echo "--> assets are content-hashed and cached forever"
asset="$(grep -o '/ui/assets/[A-Za-z0-9._-]*\.js' <<<"$index" | awk 'NR==1')"
[[ -n "$asset" ]] || { echo "the document references no script: $index" >&2; exit 1; }
headers="$(client_curl -sS -o /dev/null -D - "$BASE$asset")"
grep -qi '^cache-control: public, max-age=31536000, immutable' <<<"$headers" || { echo "$headers" >&2; exit 1; }
grep -qi '^content-type: text/javascript' <<<"$headers" || { echo "$headers" >&2; exit 1; }

echo "--> a warm browser revalidates an asset for free"
etag="$(grep -i '^etag:' <<<"$headers" | tr -d '\r' | cut -d' ' -f2-)"
[[ -n "$etag" ]] || { echo "no ETag on $asset" >&2; exit 1; }
code="$(client_curl -sS -o /dev/null -w '%{http_code}' -H "If-None-Match: $etag" "$BASE$asset")"
[[ "$code" == "304" ]] || { echo "revalidation returned $code, want 304" >&2; exit 1; }

echo "--> a stale asset reference fails loudly instead of returning HTML"
body="$(client_curl -sS -w '\n%{http_code}' "$BASE/ui/assets/index-doesnotexist.js")"
code="$(tail -n1 <<<"$body")"
[[ "$code" == "404" ]] || { echo "missing asset returned $code, want 404" >&2; exit 1; }
if grep -q 'id="root"' <<<"$body"; then
  echo "a missing asset was answered with the document" >&2; exit 1
fi

echo "--> the console reads the same API the operator would"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' "$BASE/api/v1/status")"
[[ "$code" == "200" ]] || { echo "status returned $code" >&2; exit 1; }

echo "--> the console took nothing away from the feeds"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' \
  "$BASE/maven/central/com/example/liba/1.0.0/liba-1.0.0.pom")"
[[ "$code" == "200" ]] || { echo "feed download returned $code after mounting the console" >&2; exit 1; }

echo "OK: console"
