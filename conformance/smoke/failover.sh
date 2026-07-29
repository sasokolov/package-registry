#!/usr/bin/env bash
# Take one replica out from under a running client and see what a caller
# notices. The answer should be nothing: a replica holds no state a request
# depends on (invariant 3), so the load balancer's next attempt lands on
# another one and the request succeeds.
set -uo pipefail
BASE="$1"; CONTAINER="$2"; URL="$3"

losses=0; total=0; replicas=""
poll() {
  local deadline=$((SECONDS + $1))
  while (( SECONDS < deadline )); do
    total=$((total + 1))
    r="$(curl -sS --max-time 10 -o /dev/null -D - -w '%{http_code}' "$BASE$URL" 2>/dev/null | tr -d '\r')"
    code="$(tail -c 4 <<<"$r" | tr -dc '0-9')"
    who="$(grep -i '^x-replica' <<<"$r" | awk '{print $2}')"
    [[ -n "$who" ]] && replicas="$replicas $who"
    [[ "$code" == 200 ]] || { losses=$((losses + 1)); echo "   . request $total answered $code"; }
    sleep 0.2
  done
}

echo "--> reading with both replicas up"
poll 4
echo "--> stopping $CONTAINER gracefully (SIGTERM, the way a rolling update does)"
docker stop "$CONTAINER" >/dev/null
poll 8
echo "--> starting it again"
docker start "$CONTAINER" >/dev/null
poll 8

echo "   requests: $total, failed: $losses"
echo "   replicas seen:$(tr ' ' '\n' <<<"$replicas" | sort -u | tr '\n' ' ')"
[[ "$losses" == 0 ]]
