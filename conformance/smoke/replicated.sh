#!/usr/bin/env bash
# Check that what one site published is readable at the other, and say how it
# was answered: local means the journal brought the fact and the blob is
# here; peer means replication has not caught up and the read borrowed it.
#   usage: replicated.sh <base-url> <version>   (the version the smoke printed)
set -uo pipefail
BASE="$1"; VERSION="$2"
fail=0

check() { # check <what> <url> [grep-pattern]
  local what="$1" url="$2" pattern="${3:-}"
  local body code source
  body="$(curl -sS --max-time 60 -D "/tmp/h.$$" -o "/tmp/b.$$" -w '%{http_code}' "$url")"
  code="$body"
  source="$(tr -d '\r' < "/tmp/h.$$" | grep -i '^x-registry-source' | awk '{print $2}')"
  if [[ "$code" != 200 ]]; then
    printf '   FAIL %-28s %s (%s)\n' "$what" "$code" "$url"; fail=1; rm -f "/tmp/h.$$" "/tmp/b.$$"; return
  fi
  if [[ -n "$pattern" ]] && ! grep -q "$pattern" "/tmp/b.$$"; then
    printf '   FAIL %-28s 200 but %s is missing\n' "$what" "$pattern"; fail=1; rm -f "/tmp/h.$$" "/tmp/b.$$"; return
  fi
  printf '   ok   %-28s %s\n' "$what" "${source:-?}"
  rm -f "/tmp/h.$$" "/tmp/b.$$"
}

check "maven jar"        "$BASE/maven/maven-public/com/smoke/json-smoke/$VERSION/json-smoke-$VERSION.jar"
check "maven metadata"   "$BASE/maven/maven-public/com/smoke/json-smoke/maven-metadata.xml" "$VERSION"
check "npm packument"    "$BASE/npm/npm-public/smoke-lib" "$VERSION"
check "npm tarball"      "$BASE/npm/npm-public/smoke-lib/-/smoke-lib-$VERSION.tgz"
check "nuget versions"   "$BASE/nuget/nuget-public/v3/flat2/smoke.lib/index.json" "$VERSION"
check "nuget package"    "$BASE/nuget/nuget-public/v3/flat2/smoke.lib/$VERSION/smoke.lib.$VERSION.nupkg"
check "composer metadata" "$BASE/composer/composer-public/p2/smoke/lib.json" "$VERSION"
check "composer dist"    "$BASE/composer/composer-hosted/packages/smoke/lib/$VERSION.zip"
check "helm index"       "$BASE/helm/helm-public/index.yaml" "$VERSION"
check "helm chart"       "$BASE/helm/helm-public/charts/smoke-chart-$VERSION.tgz"
check "oci tags"         "$BASE/v2/oci/oci-public/smoke-app/tags/list" "$VERSION"
check "oci manifest"     "$BASE/v2/oci/oci-public/smoke-app/manifests/$VERSION"
exit $fail
