# Shared helpers for conformance scenarios. Sourced, not executed.

CONFORMANCE_DIR="${CONFORMANCE_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)}"
COMPOSE_PROJECT="${COMPOSE_PROJECT:-registry-conformance}"

compose() {
  local files=(-f "$CONFORMANCE_DIR/docker-compose.yml")
  if [[ -n "${COMPOSE_OVERLAY:-}" ]]; then
    files+=(-f "$COMPOSE_OVERLAY")
  fi
  docker compose "${files[@]}" -p "$COMPOSE_PROJECT" "$@"
}

# client_curl runs curl inside the compose network via the one-off "client"
# service, so scenarios talk to services by their compose DNS names.
client_curl() {
  compose run --rm -T --quiet-pull client "$@"
}

# registry_token creates a static token via the CLI and prints the secret.
registry_token() {
  local name="$1"
  compose exec -T registry registry token create -name "$name" \
    -config /etc/registry/config.yaml 2>/dev/null | tail -1
}

# wait_for_chaos retries a command until it succeeds or the deadline passes.
# Chaos scenarios inject failures and then wait for recovery, which is not
# instantaneous.
wait_for_chaos() { # <seconds> <command...>
  local deadline=$((SECONDS + $1)); shift
  while (( SECONDS < deadline )); do
    if "$@"; then
      return 0
    fi
    sleep 2
  done
  return 1
}
