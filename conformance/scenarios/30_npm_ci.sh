#!/usr/bin/env bash
# Phase 4: `npm ci` of the reference project (lockfile with integrity) through
# the registry — success; then the same install with the fake upstream
# stopped — success from cache.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

restore_upstream() {
  compose up -d --wait fake-upstream >/dev/null 2>&1 || true
}
trap restore_upstream EXIT

npm_ci() {
  compose run --rm -T npm-client sh -c '
    set -e
    cp -r /src /tmp/w && cd /tmp/w
    npm config set registry http://registry:8080/npm/npmjs/
    npm ci --no-audit --no-fund
    node -e "
      const lp = require(\"left-pad\");
      const isOdd = require(\"is-odd\");
      const util = require(\"@scope/util\");
      if (lp(7, 3).length !== 3) { throw new Error(\"left-pad broken\"); }
      if (!isOdd(3)) { throw new Error(\"is-odd broken\"); }
      if (!util.scoped) { throw new Error(\"scoped package broken\"); }
      console.log(\"packages usable\");
    "
  ' 2>&1
}

echo "--> npm ci through the registry"
out="$(npm_ci)" || { echo "$out" | tail -30; exit 1; }
grep -q "packages usable" <<<"$out" || { echo "$out" | tail -30; exit 1; }

echo "--> tarballs are cached as content-addressed blobs"
listing="$(compose exec -T minio sh -c \
  "mc alias set local http://127.0.0.1:9000 registry registry-secret >/dev/null && \
   mc ls --recursive local/registry/manifests/npmjs/")"
for want in left-pad-1.3.0.tgz is-odd-3.0.1.tgz util-2.1.0.tgz; do
  grep -q "$want" <<<"$listing" || { echo "manifest for $want missing" >&2; exit 1; }
done

echo "--> stopping fake-upstream and repeating the install"
compose stop fake-upstream >/dev/null
out="$(npm_ci)" || { echo "$out" | tail -30; exit 1; }
grep -q "packages usable" <<<"$out" || { echo "offline npm ci failed" >&2; exit 1; }

src="$(client_curl -fsS -o /dev/null -w '%header{x-registry-source}' \
  http://registry:8080/npm/npmjs/left-pad/-/left-pad-1.3.0.tgz)"
if [[ "$src" != "cache" ]]; then
  echo "tarball source = $src, want cache" >&2
  exit 1
fi

echo "npm ci ok"
