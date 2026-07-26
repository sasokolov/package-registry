#!/usr/bin/env bash
# Publish permissions: an identity without the publish right gets 403, an
# anonymous publisher gets 401, and the denial is in the audit log.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

URL=http://registry:8080/maven/hosted/com/example/nopriv/1.0.0/nopriv-1.0.0.jar

echo "--> anonymous publish is 401"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' -X PUT --data-binary 'x' "$URL")"
if [[ "$code" != "401" ]]; then
  echo "anonymous PUT returned $code, want 401" >&2
  exit 1
fi

echo "--> authenticated identity without the publish right is 403"
# The hosted feed allows only "token:ci-*"; this token has another name.
other="$(registry_token "reader-$(date +%s)")"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' -X PUT --data-binary 'x' \
  -H "Authorization: Bearer $other" "$URL")"
if [[ "$code" != "403" ]]; then
  echo "unprivileged PUT returned $code, want 403" >&2
  exit 1
fi

echo "--> the artifact was not created"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' "$URL")"
if [[ "$code" == "200" ]]; then
  echo "artifact exists after a denied publish" >&2
  exit 1
fi

echo "--> audit log records the denial with identity and feed"
audit="$(compose logs --no-log-prefix registry | grep '"log":"audit"' | grep 'publish denied' || true)"
if ! grep -q '"feed":"hosted"' <<<"$audit"; then
  echo "no audit record for the denied publish" >&2
  exit 1
fi
if ! grep -q '"identity":"token:reader-' <<<"$audit"; then
  echo "audit record lacks the identity" >&2
  exit 1
fi

echo "publish permissions ok"
