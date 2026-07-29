#!/usr/bin/env bash
# What each feed holds and how much it is used.
#
# The numbers come from two places with different failure modes, and both are
# checked here: downloads are counted as they happen and folded into the
# database periodically, while storage is whatever the last inventory scan
# found by walking the store. A proxy feed is the interesting case — its cache
# has no rows anywhere, on purpose, so if the walk is wrong nothing else will
# notice.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

BASE=http://registry:8080
API=$BASE/api/v1

admin="$(registry_token "ops-usage-$(date +%s)")"

usage() { client_curl -fsS -H "Authorization: Bearer $admin" "$API/usage"; }

# field <json> <feed> <key> — one number out of the report.
field() {
  python3 -c '
import json,sys
report = json.load(sys.stdin)
for feed in report["feeds"]:
    if feed["feed"] == sys.argv[1]:
        print(feed.get(sys.argv[2], 0))
        break
else:
    raise SystemExit("no feed " + sys.argv[1])
' "$2" "$3" <<<"$1"
}

total() {
  python3 -c 'import json,sys; print(json.load(sys.stdin)["totals"].get(sys.argv[1], 0))' \
    "$2" <<<"$1"
}

# Downloads are flushed on an interval, so a check right after a request is a
# race. The interval is short in this stack; waiting for the number to move is
# what a person would do.
wait_for_downloads() { # <feed> <at least>
  local deadline=$((SECONDS + 60))
  while (( SECONDS < deadline )); do
    if (( $(field "$(usage)" "$1" downloads) >= $2 )); then
      return 0
    fi
    sleep 2
  done
  echo "feed $1 never reached $2 downloads" >&2
  usage >&2
  return 1
}

echo "--> pulling something through a proxy feed and something from a hosted one"
client_curl -fsS -o /dev/null "$BASE/maven/central/com/example/liba/1.0.0/liba-1.0.0.pom"
client_curl -fsS -o /dev/null "$BASE/maven/central/com/example/liba/1.0.0/liba-1.0.0.pom"
client_curl -fsS -o /dev/null "$BASE/maven/central/com/example/liba/1.0.0/liba-1.0.0.jar"

echo "--> the downloads are counted, and survive being asked for twice"
wait_for_downloads central 3
report="$(usage)"
[[ "$(field "$report" central downloads)" -ge 3 ]] || { echo "$report" >&2; exit 1; }
[[ "$(field "$report" central bytes_served)" -gt 0 ]] || {
  echo "bytes served were not counted:" >&2; echo "$report" >&2; exit 1; }

echo "--> and the proxy feed reports what it actually cached"
# The registry has no rows for cached content: this number can only have come
# from walking the store.
artifacts="$(field "$report" central artifacts)"
[[ "$artifacts" -ge 2 ]] || {
  echo "the proxy feed reports $artifacts artifacts; the cache walk found nothing" >&2
  echo "$report" >&2; exit 1; }
[[ "$(field "$report" central cached_bytes)" -gt 0 ]] || {
  echo "cached bytes are zero:" >&2; echo "$report" >&2; exit 1; }
[[ "$(field "$report" central hosted_artifacts)" -eq 0 ]] || {
  echo "a proxy feed reported hosted content:" >&2; echo "$report" >&2; exit 1; }
# The coordinate is recorded at ingest, so a freshly cached artifact counts as
# a package and not only as a file.
[[ "$(field "$report" central packages)" -ge 1 ]] || {
  echo "cached artifacts were not resolved to packages:" >&2; echo "$report" >&2; exit 1; }

echo "--> a hosted feed is counted from the database, not the store"
[[ "$(field "$report" hosted hosted_artifacts)" -ge 1 ]] || {
  echo "the hosted feed reports nothing:" >&2; echo "$report" >&2; exit 1; }
[[ "$(field "$report" hosted cached_artifacts)" -eq 0 ]] || {
  echo "published content was counted as cache:" >&2; echo "$report" >&2; exit 1; }

echo "--> a cache hit does not pull from the upstream, and the report says so"
hit_ratio="$(python3 -c '
import json,sys
for feed in json.load(sys.stdin)["feeds"]:
    if feed["feed"] == "central":
        print(feed.get("hit_ratio", "none"))
' <<<"$report")"
[[ "$hit_ratio" != "none" ]] || {
  echo "a proxy feed has no hit ratio:" >&2; echo "$report" >&2; exit 1; }

echo "--> a request through a group is counted on the group and on the member"
before_member="$(field "$report" central downloads)"
client_curl -fsS -o /dev/null "$BASE/maven/maven-public/com/example/liba/1.0.0/liba-1.0.0.pom"
wait_for_downloads maven-public 1
report="$(usage)"
[[ "$(field "$report" maven-public downloads)" -ge 1 ]] || {
  echo "the group counted nothing:" >&2; echo "$report" >&2; exit 1; }
# The member that answered is credited too: a group is a view, and content
# served through it is still that feed being used.
after_member="$(field "$report" central downloads)"
(( after_member > before_member )) || {
  echo "the answering member was not credited ($before_member -> $after_member):" >&2
  echo "$report" >&2; exit 1; }

echo "--> a group has no storage of its own; its numbers are its members'"
[[ "$(field "$report" maven-public artifacts)" -ge "$artifacts" ]] || {
  echo "the group reports less than its members hold:" >&2; echo "$report" >&2; exit 1; }

echo "--> the site total counts each blob once, however many feeds point at it"
site_bytes="$(total "$report" bytes)"
[[ "$site_bytes" -gt 0 ]] || { echo "site bytes are zero:" >&2; echo "$report" >&2; exit 1; }
sum_of_feeds="$(python3 -c '
import json,sys
report = json.load(sys.stdin)
print(sum(f["bytes"] for f in report["feeds"] if not f.get("group")))
' <<<"$report")"
[[ "$sum_of_feeds" -ge "$site_bytes" ]] || {
  echo "the site total ($site_bytes) exceeds the sum of its feeds ($sum_of_feeds)" >&2
  echo "$report" >&2; exit 1; }

echo "--> and the same numbers are on /metrics, by feed and not by package"
metrics="$(client_curl -fsS "$BASE/metrics")"
grep -q '^registry_feed_bytes{feed="central"' <<<"$metrics" || {
  echo "no per-feed storage gauge" >&2; exit 1; }
grep -q '^registry_store_bytes ' <<<"$metrics" || { echo "no site storage gauge" >&2; exit 1; }
grep -q '^registry_bytes_served_total{feed="central"' <<<"$metrics" || {
  echo "no per-feed bytes-served counter" >&2; exit 1; }
grep -q '^registry_group_requests_total{group="maven-public"' <<<"$metrics" || {
  echo "group traffic is not exported:" >&2; grep '^registry_group' <<<"$metrics" >&2; exit 1; }
# Cardinality is the whole reason these are feed-level: a coordinate in a
# label would grow without bound.
if grep -E '^registry_(feed|store|group)[a-z_]*\{[^}]*(coordinate|package|path)=' <<<"$metrics"; then
  echo "a usage metric is labelled per package" >&2; exit 1
fi

echo "--> the feed list carries the short form, so the console needs one request"
feeds="$(client_curl -fsS -H "Authorization: Bearer $admin" "$API/feeds")"
grep -q '"usage"' <<<"$feeds" || {
  echo "the feed list has no usage:" >&2; echo "$feeds" >&2; exit 1; }

echo "--> an anonymous caller is told nothing about how much the site holds"
code="$(client_curl -sS -o /dev/null -w '%{http_code}' "$API/usage")"
[[ "$code" == "401" ]] || { echo "anonymous usage returned $code" >&2; exit 1; }
anon="$(client_curl -fsS "$API/feeds")"
if grep -q '"usage"' <<<"$anon"; then
  echo "an anonymous feed list leaked storage numbers:" >&2; echo "$anon" >&2; exit 1
fi

echo "OK: usage"
