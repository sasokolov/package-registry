# Shared helpers for conformance scenarios. Sourced, not executed.

CONFORMANCE_DIR="${CONFORMANCE_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)}"
COMPOSE_PROJECT="${COMPOSE_PROJECT:-registry-conformance}"

compose() {
  docker compose -f "$CONFORMANCE_DIR/docker-compose.yml" -p "$COMPOSE_PROJECT" "$@"
}

# client_curl runs curl inside the compose network via the one-off "client"
# service, so scenarios talk to services by their compose DNS names.
client_curl() {
  compose run --rm -T --quiet-pull client "$@"
}
