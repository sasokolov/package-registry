#!/usr/bin/env bash
# Check that what one site published is readable at the other, and say how it
# was answered: local means the journal brought the fact and the blob is
# here; peer means replication has not caught up and the read borrowed it.
set -uo pipefail
BASE="$1"; STAMP="$2"
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

check "maven jar"        "$BASE/maven/maven-public/com/smoke/json-smoke/1.0.$STAMP/json-smoke-1.0.$STAMP.jar"
check "maven metadata"   "$BASE/maven/maven-public/com/smoke/json-smoke/maven-metadata.xml" "1.0.$STAMP"
check "npm packument"    "$BASE/npm/npm-public/@sindresorhus%2fslugify" "3.0.$STAMP"
check "npm tarball"      "$BASE/npm/npm-public/@sindresorhus/slugify/-/slugify-3.0.$STAMP.tgz"
check "nuget versions"   "$BASE/nuget/nuget-public/v3/flat2/smoke.lib/index.json" "1.0.$STAMP"
check "nuget package"    "$BASE/nuget/nuget-public/v3/flat2/smoke.lib/1.0.$STAMP/smoke.lib.1.0.$STAMP.nupkg"
check "composer metadata" "$BASE/composer/composer-public/p2/smoke/lib.json" "1.0.$STAMP"
check "composer dist"    "$BASE/composer/composer-hosted/packages/smoke/lib/1.0.$STAMP.zip"
check "helm index"       "$BASE/helm/helm-public/index.yaml" "1.0.$STAMP"
check "helm chart"       "$BASE/helm/helm-public/charts/smoke-chart-1.0.$STAMP.tgz"
check "oci tags"         "$BASE/v2/oci/oci-public/smoke-app/tags/list" "1.0.$STAMP"
check "oci manifest"     "$BASE/v2/oci/oci-public/smoke-app/manifests/1.0.$STAMP"
exit $fail
