#!/usr/bin/env bash
# Allowlist policy on the "guarded" feed: liba passes, libb is denied with
# 403 and the audit log records identity and coordinate.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

code="$(client_curl -sS -o /dev/null -w '%{http_code}' \
  http://registry:8080/maven/guarded/com/example/liba/1.0.0/liba-1.0.0.jar)"
if [[ "$code" != "200" ]]; then
  echo "allowlisted coordinate returned $code, want 200" >&2
  exit 1
fi

code="$(client_curl -sS -o /dev/null -w '%{http_code}' \
  http://registry:8080/maven/guarded/com/example/libb/1.0.0/libb-1.0.0.jar)"
if [[ "$code" != "403" ]]; then
  echo "blocked coordinate returned $code, want 403" >&2
  exit 1
fi

echo "--> audit log contains the deny with identity and coordinate"
audit="$(compose logs --no-log-prefix registry | grep '"log":"audit"' | grep 'policy denied' || true)"
if ! grep -q 'maven:com.example:libb@1.0.0' <<<"$audit"; then
  echo "audit record with the denied coordinate not found" >&2
  exit 1
fi
if ! grep -q '"identity":"anonymous:anonymous"' <<<"$audit"; then
  echo "audit record lacks the identity" >&2
  exit 1
fi
if ! grep -q '"policy":"allowlist"' <<<"$audit"; then
  echo "audit record lacks the policy attribution" >&2
  exit 1
fi

echo "allowlist policy ok"
