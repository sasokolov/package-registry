# Helpers for the geo conformance scenarios. Sourced, not executed.

GEO_DIR="${GEO_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)}"
COMPOSE_PROJECT="${COMPOSE_PROJECT:-registry-geo}"

compose() {
  docker compose -f "$GEO_DIR/docker-compose.yml" -p "$COMPOSE_PROJECT" "$@"
}

# geo_curl runs curl inside the compose network.
geo_curl() {
  compose run --rm -T --quiet-pull client "$@"
}

# site_url renders a site's public base URL.
site_url() { # <eu|us>
  echo "http://registry-$1:8080"
}

# geo_token creates a static token on a site and prints the secret.
geo_token() { # <eu|us> <name>
  compose exec -T "registry-$1" registry token create \
    -name "$2" -config /etc/registry/config.yaml 2>/dev/null | tail -1
}

# publish PUTs content to a site and prints the HTTP status.
publish() { # <eu|us> <feed> <path> <content> <token>
  geo_curl -sS -o /dev/null -w '%{http_code}' -X PUT \
    --data-binary "$4" -H "Authorization: Bearer $5" \
    "$(site_url "$1")/maven/$2/$3"
}

# fetch GETs a path from a site and prints "<status> <source>".
fetch() { # <eu|us> <feed> <path>
  geo_curl -sS -o /dev/null -w '%{http_code} %header{x-registry-source}' \
    "$(site_url "$1")/maven/$2/$3"
}

# body prints the response body of a fetch.
body() { # <eu|us> <feed> <path>
  geo_curl -sS "$(site_url "$1")/maven/$2/$3"
}

# wait_for retries a command until it succeeds or the deadline passes.
wait_for() { # <seconds> <command...>
  local deadline=$((SECONDS + $1)); shift
  while (( SECONDS < deadline )); do
    if "$@"; then
      return 0
    fi
    sleep 1
  done
  return 1
}

# replicated succeeds when a site serves the expected content locally.
replicated() { # <eu|us> <feed> <path> <expected>
  local got
  got="$(body "$1" "$2" "$3" 2>/dev/null || true)"
  [[ "$got" == "$4" ]]
}

# repl_status prints a site's replication status (operator view).
repl_status() { # <eu|us>
  compose exec -T "registry-$1" registry repl status -config /etc/registry/config.yaml 2>/dev/null
}

# The sites are linked by a dedicated "wan" network; a partition is a
# disconnect from it, which needs no tooling inside the containers and
# leaves each site's own storage and database reachable.
wan_network() { echo "${COMPOSE_PROJECT}_wan"; }
us_container() { compose ps -q registry-us | head -1; }

# partition cuts the link between the sites.
partition() {
  docker network disconnect "$(wan_network)" "$(us_container)" 2>/dev/null || true
}

# heal restores it.
heal() {
  docker network connect --alias registry-us "$(wan_network)" "$(us_container)" 2>/dev/null || true
}
