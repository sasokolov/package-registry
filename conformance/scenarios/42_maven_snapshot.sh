#!/usr/bin/env bash
# Phase 5: deploying a SNAPSHOT twice — the second build becomes the current
# one in the version-level maven-metadata.xml, and both timestamped builds
# remain retrievable.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

token="$(fondaco_token "ci-snapshot-$(date +%s)")"
BASE=http://registry:8080/maven/snapshots/com/example/snap/1.0.0-SNAPSHOT

deploy_build() { # <timestamp> <build-number> <content>
  local stamp="$1" build="$2" content="$3"
  local jar="snap-1.0.0-${stamp}-${build}.jar"
  local pom="snap-1.0.0-${stamp}-${build}.pom"
  client_curl -fsS -o /dev/null -X PUT --data-binary "$content" \
    -H "Authorization: Bearer $token" "$BASE/$jar"
  client_curl -fsS -o /dev/null -X PUT \
    --data-binary "<project><groupId>com.example</groupId><artifactId>snap</artifactId><version>1.0.0-SNAPSHOT</version></project>" \
    -H "Authorization: Bearer $token" "$BASE/$pom"
}

echo "--> deploying SNAPSHOT build 1"
deploy_build 20260726.101500 1 "snapshot build one"

meta="$(client_curl -fsS "$BASE/maven-metadata.xml")"
grep -q "<buildNumber>1</buildNumber>" <<<"$meta" || { echo "$meta" >&2; exit 1; }

echo "--> deploying SNAPSHOT build 2"
deploy_build 20260726.120000 2 "snapshot build two"

meta="$(client_curl -fsS "$BASE/maven-metadata.xml")"
grep -q "<buildNumber>2</buildNumber>" <<<"$meta" || {
  echo "second deploy did not become current:" >&2; echo "$meta" >&2; exit 1; }
grep -q "<timestamp>20260726.120000</timestamp>" <<<"$meta" || { echo "$meta" >&2; exit 1; }
grep -q "<value>1.0.0-20260726.120000-2</value>" <<<"$meta" || { echo "$meta" >&2; exit 1; }

echo "--> both timestamped builds stay retrievable (immutable artifacts)"
body1="$(client_curl -fsS "$BASE/snap-1.0.0-20260726.101500-1.jar")"
body2="$(client_curl -fsS "$BASE/snap-1.0.0-20260726.120000-2.jar")"
if [[ "$body1" != "snapshot build one" || "$body2" != "snapshot build two" ]]; then
  echo "timestamped builds are not served verbatim: $body1 / $body2" >&2
  exit 1
fi

echo "--> re-deploying the same timestamped build with different content is 409"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' -X PUT --data-binary "tampered" \
  -H "Authorization: Bearer $token" "$BASE/snap-1.0.0-20260726.101500-1.jar")"
if [[ "$code" != "409" ]]; then
  echo "immutability of a timestamped build not enforced: $code" >&2
  exit 1
fi

echo "maven snapshot ok"
