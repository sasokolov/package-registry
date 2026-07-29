#!/usr/bin/env bash
# Smoke a RUNNING registry with real clients: every format, every kind of
# feed. Each format does the same three things — resolve something real
# through the group (which is the proxy path), publish something of its own
# into the hosted feed, and resolve that back through the group.
#
# This is deliberately not hermetic, which is the point: conformance proves
# the protocols against fixtures, and this proves a deployment against the
# internet, the clients people actually run, and whatever is in front of it.
# Everything it publishes carries a timestamp, so it can be run against the
# same stand as often as you like.
#
#   usage: conformance/smoke/run.sh <base-url> <publish-token> <label> [formats...]
#
# On a site that does not own the hosted feeds (geo write-affinity) a publish
# is forwarded to the home site and comes back through the journal; the
# read-back steps wait for that and report how long it took.
set -uo pipefail

BASE="$1"; TOKEN="$2"; LABEL="$3"; shift 3
FORMATS=("${@:-}")
[[ -z "${FORMATS[*]}" ]] && FORMATS=(maven npm nuget composer terraform helm oci)

HOSTPORT="${BASE#http://}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK="${SMOKE_WORK:-$ROOT/.work-$LABEL}"
SRC="$WORK/src"
STAMP="${SMOKE_STAMP:-$(date +%s)}"
mkdir -p "$WORK" "$SRC"

declare -A RESULT
UID_GID="$(id -u):$(id -g)"

# fixtures makes what the clients need, once per work directory: a real
# project to build (so the proxy path carries a real dependency tree) and
# small packages of our own to publish. Nothing is vendored into the
# repository — a smoke that ships its own copy of somebody's library is a
# smoke that stops resembling what it is testing.
fixtures() {
  if [[ ! -d "$SRC/json-java" ]]; then
    echo "--> fetching a real project to build (once)"
    git clone --depth 1 -q https://github.com/stleary/JSON-java.git "$SRC/json-java" \
      || { bad "cannot clone the project to build"; return 1; }
  fi

  mkdir -p "$SRC/npm/pkg" "$SRC/npm/consumer"
  cat > "$SRC/npm/pkg/package.json" <<'EOF'
{ "name": "smoke-lib", "version": "0.0.0", "license": "MIT",
  "description": "published by the registry smoke", "main": "index.js" }
EOF
  echo 'module.exports = () => "hello from the smoke library";' > "$SRC/npm/pkg/index.js"
  # A real dependency tree, so the install goes through the proxy for
  # something the registry did not publish itself.
  cat > "$SRC/npm/consumer/package.json" <<'EOF'
{ "name": "smoke-consumer", "version": "1.0.0", "private": true,
  "dependencies": { "chalk": "^5.3.0", "escape-string-regexp": "^5.0.0" } }
EOF

  mkdir -p "$SRC/nuget/lib" "$SRC/nuget/app"
  cat > "$SRC/nuget/lib/Smoke.Lib.csproj" <<'EOF'
<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup>
    <TargetFramework>net8.0</TargetFramework>
    <PackageId>Smoke.Lib</PackageId>
    <Authors>smoke</Authors>
    <Description>A library published by the registry smoke.</Description>
    <PackageLicenseExpression>MIT</PackageLicenseExpression>
  </PropertyGroup>
</Project>
EOF
  cat > "$SRC/nuget/lib/Lib.cs" <<'EOF'
namespace Smoke;

public static class Lib
{
    public static string Greet() => "hello from the smoke library";
}
EOF
  cat > "$SRC/nuget/app/App.csproj" <<'EOF'
<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup><OutputType>Exe</OutputType><TargetFramework>net8.0</TargetFramework></PropertyGroup>
  <ItemGroup><PackageReference Include="Newtonsoft.Json" Version="13.0.3" /></ItemGroup>
</Project>
EOF
  cat > "$SRC/nuget/app/Program.cs" <<'EOF'
using Newtonsoft.Json;

System.Console.WriteLine(JsonConvert.SerializeObject(new { smoke = true }));
EOF
}

say() { printf '\n\033[1m== %s: %s\033[0m\n' "$LABEL" "$*"; }
ok()  { printf '   ok  %s\n' "$*"; }
bad() { printf '   FAIL %s\n' "$*" >&2; }

run() { # run <image> <workdir-in-container> <cmd...>
  local image="$1" wd="$2"; shift 2
  docker run --rm --network host --user "$UID_GID" \
    -e HOME=/w -v "$WORK:/w" -w "$wd" "$image" "$@"
}

curl_reg() { curl -sS --max-time 120 "$@"; }

# settle waits until a document served here mentions what was just published.
#
# On the site that owns a hosted feed this returns at once. On any other site
# it is the journal round trip: a publish is forwarded to the home site, comes
# back as a fact, and only then does this site rebuild the indexes derived
# from it. An exact coordinate is readable immediately (the read path borrows
# it from the peer); a version LIST is not, and pretending otherwise would be
# testing a registry nobody deployed.
settle() { # settle <url> <pattern> [seconds]
  local url="$1" pattern="$2" limit="${3:-90}" waited=0
  while (( waited < limit )); do
    if curl -sS --max-time 30 "$url" 2>/dev/null | grep -q "$pattern"; then
      (( waited > 0 )) && ok "it appeared here ${waited}s after the publish"
      return 0
    fi
    sleep 1; waited=$((waited + 1))
  done
  bad "still not visible at $url after ${limit}s"
  return 1
}


# --------------------------------------------------------------------- maven
smoke_maven() {
  say "maven"
  local repo="$WORK/m2-$STAMP" ver="1.0.$STAMP"
  rm -rf "$repo" "$WORK/json-java"; mkdir -p "$repo"
  cp -r "$SRC/json-java" "$WORK/json-java"
  cat > "$WORK/settings.xml" <<EOF
<settings>
  <servers><server><id>registry</id><username>ci</username><password>$TOKEN</password></server></servers>
  <mirrors><mirror><id>registry</id><url>$BASE/maven/maven-public</url><mirrorOf>*</mirrorOf></mirror></mirrors>
</settings>
EOF
  run maven:3-eclipse-temurin-21 /w/json-java \
    mvn -B -q -s /w/settings.xml -Dmaven.repo.local=/w/m2-$STAMP -DskipTests package \
    > "$WORK/maven-build.log" 2>&1 || { bad "build through maven-public"; tail -5 "$WORK/maven-build.log"; return 1; }
  ok "built json-java through maven-public (every dependency from the registry)"

  local jar; jar="$(ls "$WORK"/json-java/target/*.jar | grep -v sources | head -1)"
  run maven:3-eclipse-temurin-21 /w/json-java \
    mvn -B -q -s /w/settings.xml -Dmaven.repo.local=/w/m2-$STAMP deploy:deploy-file \
      -Dfile="/w/json-java/$(basename "$(dirname "$jar")")/$(basename "$jar")" \
      -DgroupId=com.smoke -DartifactId=json-smoke -Dversion="$ver" -Dpackaging=jar \
      -DrepositoryId=registry -Durl="$BASE/maven/releases" \
    > "$WORK/maven-deploy.log" 2>&1 || { bad "deploy to releases"; tail -5 "$WORK/maven-deploy.log"; return 1; }
  ok "deployed com.smoke:json-smoke:$ver to the hosted feed"

  rm -rf "$WORK/m2-back-$STAMP"
  run maven:3-eclipse-temurin-21 /w \
    mvn -B -q -s /w/settings.xml -Dmaven.repo.local=/w/m2-back-$STAMP \
      dependency:get -Dartifact="com.smoke:json-smoke:$ver" \
    > "$WORK/maven-resolve.log" 2>&1 || { bad "resolve it back through the group"; tail -5 "$WORK/maven-resolve.log"; return 1; }
  ok "resolved it back through maven-public (empty local repo)"
}

# ----------------------------------------------------------------------- npm
smoke_npm() {
  say "npm"
  local ver="1.0.$STAMP"
  rm -rf "$WORK/npm"; mkdir -p "$WORK/npm"
  cp -r "$SRC/npm/pkg" "$SRC/npm/consumer" "$WORK/npm/"
  rm -rf "$WORK/npm/pkg/node_modules" "$WORK/npm/consumer/node_modules"
  cat > "$WORK/.npmrc" <<EOF
registry=$BASE/npm/npm-public/
//$HOSTPORT/npm/npm-hosted/:_authToken=$TOKEN
//$HOSTPORT/npm/npm-public/:_authToken=$TOKEN
EOF
  run node:22-alpine /w/npm/consumer npm install --no-audit --no-fund --cache /w/npmcache \
    > "$WORK/npm-install.log" 2>&1 || { bad "install through npm-public"; tail -5 "$WORK/npm-install.log"; return 1; }
  ok "installed the consumer's tree through npm-public"

  sed -i "s/\"version\": \"[^\"]*\"/\"version\": \"$ver\"/" "$WORK/npm/pkg/package.json"
  run node:22-alpine /w/npm/pkg npm publish --registry "$BASE/npm/npm-hosted/" \
    > "$WORK/npm-publish.log" 2>&1 || { bad "publish to npm-hosted"; tail -5 "$WORK/npm-publish.log"; return 1; }
  ok "published smoke-lib@$ver to the hosted feed"

  settle "$BASE/npm/npm-public/smoke-lib" "$ver" || return 1
  rm -rf "$WORK/npm/back"; mkdir -p "$WORK/npm/back"
  run node:22-alpine /w/npm/back npm install --no-audit --no-fund --cache /w/npmcache-back \
      "smoke-lib@$ver" \
    > "$WORK/npm-back.log" 2>&1 || { bad "install it back through npm-public"; tail -5 "$WORK/npm-back.log"; return 1; }
  ok "installed smoke-lib@$ver back through npm-public"
}

# --------------------------------------------------------------------- nuget
smoke_nuget() {
  say "nuget"
  local ver="1.0.$STAMP"
  rm -rf "$WORK/nuget"; mkdir -p "$WORK/nuget"
  cp -r "$SRC/nuget/lib" "$SRC/nuget/app" "$WORK/nuget/"
  rm -rf "$WORK/nuget/lib/bin" "$WORK/nuget/lib/obj" "$WORK/nuget/app/bin" "$WORK/nuget/app/obj"
  cat > "$WORK/nuget/nuget.config" <<EOF
<?xml version="1.0" encoding="utf-8"?>
<configuration>
  <packageSources>
    <clear />
    <add key="registry" value="$BASE/nuget/nuget-public/v3/index.json" />
  </packageSources>
</configuration>
EOF
  cp "$WORK/nuget/nuget.config" "$WORK/nuget/lib/" ; cp "$WORK/nuget/nuget.config" "$WORK/nuget/app/"

  run mcr.microsoft.com/dotnet/sdk:8.0 /w/nuget/app \
    dotnet restore --no-cache \
    > "$WORK/nuget-restore.log" 2>&1 || { bad "restore through nuget-public"; tail -5 "$WORK/nuget-restore.log"; return 1; }
  ok "restored the app's dependencies through nuget-public"

  run mcr.microsoft.com/dotnet/sdk:8.0 /w/nuget/lib \
    dotnet pack -c Release -p:PackageVersion="$ver" -o /w/nuget/out \
    > "$WORK/nuget-pack.log" 2>&1 || { bad "pack the library"; tail -5 "$WORK/nuget-pack.log"; return 1; }
  run mcr.microsoft.com/dotnet/sdk:8.0 /w/nuget \
    dotnet nuget push "/w/nuget/out/Smoke.Lib.$ver.nupkg" \
      --source "$BASE/nuget/nuget-hosted/v3/index.json" --api-key "$TOKEN" \
    > "$WORK/nuget-push.log" 2>&1 || { bad "push to nuget-hosted"; tail -5 "$WORK/nuget-push.log"; return 1; }
  ok "pushed Smoke.Lib $ver to the hosted feed"

  settle "$BASE/nuget/nuget-public/v3/flat2/smoke.lib/index.json" "$ver" || return 1
  local versions
  versions="$(curl_reg "$BASE/nuget/nuget-public/v3/flat2/smoke.lib/index.json")"
  grep -q "$ver" <<<"$versions" || { bad "the pushed version is not in the group's version list: $versions"; return 1; }
  ok "the group's flat container lists $ver"

  rm -rf "$WORK/nuget/back"; mkdir -p "$WORK/nuget/back"
  cp "$WORK/nuget/nuget.config" "$WORK/nuget/back/"
  cat > "$WORK/nuget/back/Back.csproj" <<EOF2
<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup><OutputType>Exe</OutputType><TargetFramework>net8.0</TargetFramework></PropertyGroup>
  <ItemGroup><PackageReference Include="Smoke.Lib" Version="$ver" /></ItemGroup>
</Project>
EOF2
  echo 'System.Console.WriteLine(Smoke.Lib.Greet());' > "$WORK/nuget/back/Program.cs"
  # --no-cache: NuGet keeps its own HTTP cache of index documents for half an
  # hour, and a version published a second ago is exactly what it would miss.
  run mcr.microsoft.com/dotnet/sdk:8.0 /w/nuget/back \
    sh -c "dotnet restore --no-cache && dotnet run --no-restore" \
    > "$WORK/nuget-back.log" 2>&1 || { bad "consume the published package through nuget-public"; tail -8 "$WORK/nuget-back.log"; return 1; }
  grep -q "hello from the smoke library" "$WORK/nuget-back.log" || { bad "the published library did not run"; tail -5 "$WORK/nuget-back.log"; return 1; }
  ok "an app restored and ran Smoke.Lib $ver from the hosted feed"
}

# ------------------------------------------------------------------ composer
smoke_composer() {
  say "composer"
  local ver="1.0.$STAMP"
  rm -rf "$WORK/composer"; mkdir -p "$WORK/composer/app" "$WORK/composer/home"
  cat > "$WORK/composer/app/composer.json" <<EOF
{ "name": "smoke/consumer",
  "repositories": [ {"type":"composer","url":"$BASE/composer/composer-public"}, {"packagist.org": false} ],
  "require": { "nikic/iter": "2.4.0" },
  "config": { "secure-http": false } }
EOF
  run composer:2 /w/composer/app composer install --no-interaction --no-progress \
    > "$WORK/composer-install.log" 2>&1 || { bad "install through composer-public"; tail -5 "$WORK/composer-install.log"; return 1; }
  ok "installed nikic/iter through composer-public"

  # A package of our own: a zip with a composer.json at its root.
  rm -rf "$WORK/composer/pkg"; mkdir -p "$WORK/composer/pkg/smoke-lib"
  cat > "$WORK/composer/pkg/smoke-lib/composer.json" <<EOF
{ "name": "smoke/lib", "version": "$ver", "description": "smoke", "license": "MIT",
  "autoload": { "psr-4": { "Smoke\\\\": "src/" } } }
EOF
  mkdir -p "$WORK/composer/pkg/smoke-lib/src"
  echo '<?php namespace Smoke; class Lib { public static function hi(): string { return "hi"; } }' \
    > "$WORK/composer/pkg/smoke-lib/src/Lib.php"
  run composer:2 /w/composer/pkg sh -c "cd smoke-lib && zip -qr ../smoke-lib.zip ." \
    > "$WORK/composer-zip.log" 2>&1 || { bad "package the zip"; tail -3 "$WORK/composer-zip.log"; return 1; }

  local code
  code="$(curl_reg -o "$WORK/composer-publish.out" -w '%{http_code}' -X PUT \
    -H "Authorization: Bearer $TOKEN" --data-binary "@$WORK/composer/pkg/smoke-lib.zip" \
    "$BASE/composer/composer-hosted/packages/smoke/lib/$ver.zip")"
  [[ "$code" == 201 || "$code" == 200 ]] || { bad "publish to composer-hosted: $code $(cat "$WORK/composer-publish.out")"; return 1; }
  ok "published smoke/lib $ver to the hosted feed"

  settle "$BASE/composer/composer-public/p2/smoke/lib.json" "$ver" || return 1
  rm -rf "$WORK/composer/back"; mkdir -p "$WORK/composer/back"
  cat > "$WORK/composer/back/composer.json" <<EOF
{ "name": "smoke/back",
  "repositories": [ {"type":"composer","url":"$BASE/composer/composer-public"}, {"packagist.org": false} ],
  "require": { "smoke/lib": "$ver" },
  "config": { "secure-http": false } }
EOF
  run composer:2 /w/composer/back composer install --no-interaction --no-progress \
    > "$WORK/composer-back.log" 2>&1 || { bad "install it back through composer-public"; tail -5 "$WORK/composer-back.log"; return 1; }
  ok "installed smoke/lib $ver back through composer-public"
}

# ----------------------------------------------------------------- terraform
smoke_terraform() {
  say "terraform"
  # The real CLI insists on HTTPS for the module protocol, and this stand is
  # plain HTTP; conformance drives it through a TLS gateway. What is checked
  # here is the protocol itself, end to end: discovery, versions, and the
  # download indirection every client follows.
  local disco versions headers
  disco="$(curl_reg "$BASE/.well-known/terraform.json")"
  grep -q "modules.v1" <<<"$disco" || { bad "service discovery: $disco"; return 1; }
  ok "service discovery points at the terraform feed"

  versions="$(curl_reg "$BASE/terraform/tf-public/v1/modules/terraform-aws-modules/vpc/aws/versions")"
  grep -q '"versions"' <<<"$versions" || { bad "versions: ${versions:0:200}"; return 1; }
  ok "the group answers a real module's version list from the upstream"

  headers="$(curl_reg -o /dev/null -D - \
    "$BASE/terraform/tf-public/v1/modules/terraform-aws-modules/vpc/aws/5.0.0/download" | tr -d '\r')"
  grep -qi "^x-terraform-get:" <<<"$headers" || { bad "no download indirection: $headers"; return 1; }
  ok "the download indirection points back at this registry"
}

# ---------------------------------------------------------------------- helm
smoke_helm() {
  say "helm"
  local ver="1.0.$STAMP"
  rm -rf "$WORK/helm"; mkdir -p "$WORK/helm"

  docker run --rm --network host --user "$UID_GID" -v "$WORK/helm:/tmp/h" \
    -e HELM_CACHE_HOME=/tmp/h/cache -e HELM_CONFIG_HOME=/tmp/h/config -e HELM_DATA_HOME=/tmp/h/data \
    --entrypoint sh alpine/helm:3.16.3 -c "
      set -e
      helm repo add public $BASE/helm/helm-public >/dev/null
      helm repo update public >/dev/null
      helm search repo public | head -20
    " > "$WORK/helm-search.log" 2>&1 || { bad "search through helm-public"; tail -5 "$WORK/helm-search.log"; return 1; }
  grep -q "public/" "$WORK/helm-search.log" || { bad "nothing in the group index"; cat "$WORK/helm-search.log"; return 1; }
  ok "the group index carries the proxied charts ($(grep -c 'public/' "$WORK/helm-search.log") entries)"

  docker run --rm --network host --user "$UID_GID" -v "$WORK/helm:/tmp/h" \
    -e HELM_CACHE_HOME=/tmp/h/cache -e HELM_CONFIG_HOME=/tmp/h/config -e HELM_DATA_HOME=/tmp/h/data \
    --entrypoint sh alpine/helm:3.16.3 -c "
      set -e
      cd /tmp/h
      helm create smoke-chart >/dev/null
      sed -i 's/^version: .*/version: $ver/' smoke-chart/Chart.yaml
      sed -i 's/^description: .*/description: smoke $LABEL/' smoke-chart/Chart.yaml
      helm package smoke-chart -d /tmp/h >/dev/null
      wget -q -O- --method=POST --header='Authorization: Bearer $TOKEN' \
        --body-file=/tmp/h/smoke-chart-$ver.tgz $BASE/helm/helm-hosted/api/charts && echo UPLOADED
    " > "$WORK/helm-push.log" 2>&1 || { bad "push to helm-hosted"; tail -5 "$WORK/helm-push.log"; return 1; }
  grep -q UPLOADED "$WORK/helm-push.log" || { bad "push to helm-hosted"; cat "$WORK/helm-push.log"; return 1; }
  ok "published smoke-chart $ver to the hosted feed"

  settle "$BASE/helm/helm-public/index.yaml" "$ver" || return 1
  docker run --rm --network host --user "$UID_GID" -v "$WORK/helm:/tmp/h" \
    -e HELM_CACHE_HOME=/tmp/h/cache2 -e HELM_CONFIG_HOME=/tmp/h/config2 -e HELM_DATA_HOME=/tmp/h/data2 \
    --entrypoint sh alpine/helm:3.16.3 -c "
      set -e
      helm repo add public $BASE/helm/helm-public >/dev/null
      helm repo update public >/dev/null
      helm template r public/smoke-chart --version $ver | head -5
      mkdir -p /tmp/h/pulled
      helm pull public/smoke-chart --version $ver -d /tmp/h/pulled
      ls /tmp/h/pulled
    " > "$WORK/helm-back.log" 2>&1 || { bad "install it back through helm-public"; tail -8 "$WORK/helm-back.log"; return 1; }
  grep -q "smoke-chart-$ver.tgz" "$WORK/helm-back.log" || { bad "pull back"; tail -8 "$WORK/helm-back.log"; return 1; }
  ok "rendered and pulled it back through helm-public"
}

# ----------------------------------------------------------------------- oci
smoke_oci() {
  say "oci"
  local tag="1.0.$STAMP"
  local proxied="$HOSTPORT/oci/dockerhub/library/busybox:1.36"
  local mine="$HOSTPORT/oci/images/smoke-app:$tag"
  local via_group="$HOSTPORT/oci/oci-public/smoke-app:$tag"

  docker rmi -f "$proxied" "$mine" "$via_group" >/dev/null 2>&1
  timeout 300 docker pull "$proxied" > "$WORK/oci-pull.log" 2>&1 \
    || { bad "pull through the proxy feed"; tail -5 "$WORK/oci-pull.log"; return 1; }
  ok "pulled busybox:1.36 through the Docker Hub proxy"

  echo "$TOKEN" | timeout 120 docker login "$HOSTPORT" -u ci --password-stdin > "$WORK/oci-login.log" 2>&1 \
    || { bad "docker login"; tail -3 "$WORK/oci-login.log"; return 1; }
  docker tag "$proxied" "$mine"
  timeout 300 docker push "$mine" > "$WORK/oci-push.log" 2>&1 \
    || { bad "push to the hosted feed"; tail -5 "$WORK/oci-push.log"; return 1; }
  ok "pushed smoke-app:$tag to the hosted feed"

  settle "$BASE/v2/oci/oci-public/smoke-app/tags/list" "$tag" || return 1
  docker rmi -f "$mine" >/dev/null 2>&1
  timeout 300 docker pull "$via_group" > "$WORK/oci-back.log" 2>&1 \
    || { bad "pull it back through the group"; tail -5 "$WORK/oci-back.log"; return 1; }
  ok "pulled it back through oci-public"

  local tags
  tags="$(curl_reg "$BASE/v2/oci/oci-public/smoke-app/tags/list")"
  grep -q "$tag" <<<"$tags" || { bad "the group's tag list: $tags"; return 1; }
  ok "the group lists the tag: $(python3 -c "import json,sys;print(','.join(json.loads(sys.argv[1])['tags'][-3:]))" "$tags")"
  docker rmi -f "$via_group" "$proxied" >/dev/null 2>&1
}

fixtures || exit 1

for format in "${FORMATS[@]}"; do
  if "smoke_$format"; then RESULT[$format]=ok; else RESULT[$format]=FAIL; fi
done

printf '\n\033[1m== %s: summary (stamp %s)\033[0m\n' "$LABEL" "$STAMP"
failed=0
fixtures || exit 1

for format in "${FORMATS[@]}"; do
  printf '   %-10s %s\n' "$format" "${RESULT[$format]}"
  [[ "${RESULT[$format]}" == ok ]] || failed=1
done
exit $failed
