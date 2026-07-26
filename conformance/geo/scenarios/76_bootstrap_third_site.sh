#!/usr/bin/env bash
# Geo scenario 7: an empty third site joins the mesh. It must bootstrap
# from a peer snapshot, serve hosted content through peer-fallback before
# its own blobs arrive, and backfill them on demand.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

stamp="$(date +%s)"
token="$(geo_token eu "ci-boot-$stamp")"
PATH_JAR="com/example/boot/1.0.0/boot-1.0.0.jar"
CONTENT="content that must reach a brand new site"

echo "--> publishing at eu-1 before the third site exists"
code="$(publish eu homed "$PATH_JAR" "$CONTENT" "$token")"
[[ "$code" == "201" ]] || { echo "publish returned $code" >&2; exit 1; }

cleanup_third() {
  compose --profile third stop registry-ap minio-ap postgres-ap >/dev/null 2>&1 || true
  compose --profile third rm -f registry-ap minio-ap postgres-ap >/dev/null 2>&1 || true
}
trap cleanup_third EXIT

echo "--> starting an empty third site (ap-1)"
compose --profile third up -d --build --wait --wait-timeout 300 registry-ap >/dev/null

echo "--> ap-1 serves the hosted artifact (peer fallback, then locally)"
serves_it() {
  local out
  out="$(body ap homed "$PATH_JAR" 2>/dev/null)" || return 1
  [[ "$out" == "$CONTENT" ]]
}
if ! wait_for 120 serves_it; then
  echo "the new site never served the pre-existing artifact" >&2
  compose exec -T registry-ap registry repl status -config /etc/registry/config.yaml >&2 || true
  exit 1
fi

echo "--> the first answer may come from a peer; the source header says so"
read -r status source <<<"$(fetch ap homed "$PATH_JAR")"
[[ "$status" == "200" ]] || { echo "ap-1 returned $status" >&2; exit 1; }
case "$source" in
  peer|cache|local) echo "    served from: $source" ;;
  *) echo "unexpected source $source" >&2; exit 1 ;;
esac

echo "--> ap-1 knows both peers and has cursors for both journals"
ap_status="$(compose exec -T registry-ap registry repl status -config /etc/registry/config.yaml 2>/dev/null)"
for peer in eu-1 us-1; do
  grep -q "$peer" <<<"$ap_status" || {
    echo "ap-1 has no stream for $peer" >&2; echo "$ap_status" >&2; exit 1; }
done

echo "--> backfill reports (and then fixes) blobs the lazy feed has not pulled"
report="$(compose exec -T registry-ap registry repl backfill -config /etc/registry/config.yaml 2>/dev/null)"
if grep -q 'blob(s) missing' <<<"$report"; then
  compose exec -T registry-ap registry repl backfill -dry-run=false \
    -config /etc/registry/config.yaml >/dev/null
  after="$(compose exec -T registry-ap registry repl backfill -config /etc/registry/config.yaml 2>/dev/null)"
  grep -q 'every hosted coordinate has its blob locally' <<<"$after" || {
    echo "backfill did not fetch every missing blob:" >&2; echo "$after" >&2; exit 1; }
fi

echo "--> after backfill the artifact is served from local storage"
locally() {
  read -r s src <<<"$(fetch ap homed "$PATH_JAR")"
  [[ "$s" == "200" && "$src" == "cache" ]]
}
if ! wait_for 60 locally; then
  echo "the new site still depends on peers after backfill" >&2
  exit 1
fi

echo "--> a publish at eu-1 now reaches all three sites"
NEW_PATH="com/example/boot/2.0.0/boot-2.0.0.jar"
code="$(publish eu homed "$NEW_PATH" "second release" "$token")"
[[ "$code" == "201" ]] || { echo "second publish returned $code" >&2; exit 1; }
for site in us ap; do
  if ! wait_for 90 replicated "$site" homed "$NEW_PATH" "second release"; then
    echo "site $site never received the second release" >&2
    exit 1
  fi
done

echo "third-site bootstrap ok"
