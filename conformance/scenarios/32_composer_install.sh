#!/usr/bin/env bash
# Phase 4: `composer install` of the reference project through the registry —
# success; repeated with the fake upstream stopped — success from cache.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

restore_upstream() {
  compose up -d --wait fake-upstream >/dev/null 2>&1 || true
}
trap restore_upstream EXIT

composer_install() {
  compose run --rm -T composer-client sh -c '
    set -e
    cp -r /src /tmp/w && cd /tmp/w
    composer install --no-interaction --no-progress --no-cache
    test -f vendor/acme/lib-a/src/a.php
    test -f vendor/acme/lib-b/src/b.php
    php -r "require \"vendor/autoload.php\"; echo acme_a(), acme_b(), PHP_EOL;"
  ' 2>&1
}

echo "--> composer install through the registry"
out="$(composer_install)" || { echo "$out" | tail -30; exit 1; }
grep -q "^ab$" <<<"$out" || { echo "autoloaded classes did not work:" >&2; echo "$out" | tail -20 >&2; exit 1; }

echo "--> dists are cached"
listing="$(compose exec -T minio sh -c \
  "mc alias set local http://127.0.0.1:9000 registry registry-secret >/dev/null && \
   mc ls --recursive local/registry/manifests/packagist/")"
grep -q "lib-a" <<<"$listing" || { echo "lib-a dist not cached" >&2; exit 1; }
grep -q "lib-b" <<<"$listing" || { echo "lib-b dist not cached" >&2; exit 1; }

echo "--> stopping fake-upstream and repeating the install"
compose stop fake-upstream >/dev/null
out="$(composer_install)" || { echo "$out" | tail -30; exit 1; }
grep -q "^ab$" <<<"$out" || { echo "offline composer install failed" >&2; exit 1; }

echo "composer install ok"
