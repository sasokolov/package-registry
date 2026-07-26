#!/usr/bin/env bash
# Phase 5: `npm publish` into a hosted feed from an identity with the publish
# right — success, package installable; republishing the same version — 409.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

token="$(registry_token "ci-npm-$(date +%s)")"
REG=http://registry:8080/npm/npm-hosted/

publish() { # <version> <extra-file-content>
  compose run --rm -T -e NPM_TOKEN="$token" npm-client sh -c "
    set -e
    rm -rf /tmp/p && mkdir -p /tmp/p && cd /tmp/p
    cat > package.json <<'JSON'
{
  \"name\": \"hosted-pkg\",
  \"version\": \"$1\",
  \"main\": \"index.js\",
  \"license\": \"MIT\"
}
JSON
    echo 'module.exports = \"$2\";' > index.js
    npm config set registry $REG
    npm config set //registry:8080/npm/npm-hosted/:_authToken \$NPM_TOKEN
    npm publish
  " 2>&1
}

echo "--> npm publish"
out="$(publish 1.0.0 first)" || { echo "$out" | tail -30; exit 1; }
grep -q "hosted-pkg@1.0.0" <<<"$out" || { echo "$out" | tail -20; exit 1; }

echo "--> the published package is installable"
out="$(compose run --rm -T npm-client sh -c "
  set -e
  rm -rf /tmp/i && mkdir -p /tmp/i && cd /tmp/i
  npm config set registry $REG
  npm install hosted-pkg@1.0.0 --no-audit --no-fund
  node -e \"if (require('hosted-pkg') !== 'first') { throw new Error('wrong content'); } console.log('install ok')\"
" 2>&1)" || { echo "$out" | tail -30; exit 1; }
grep -q "install ok" <<<"$out" || { echo "$out" | tail -20; exit 1; }

echo "--> republishing the same version with different content is refused"
out="$(publish 1.0.0 second || true)"
if ! grep -qE "409|Conflict|immutable|cannot publish over" <<<"$out"; then
  echo "republish was not refused:" >&2
  echo "$out" | tail -20 >&2
  exit 1
fi

echo "--> a new version publishes and becomes latest"
out="$(publish 1.1.0 newer)" || { echo "$out" | tail -30; exit 1; }
tags="$(client_curl -fsS http://registry:8080/npm/npm-hosted/-/package/hosted-pkg/dist-tags)"
grep -q '"latest": *"1.1.0"' <<<"$tags" || { echo "dist-tags: $tags" >&2; exit 1; }

echo "npm publish ok"
