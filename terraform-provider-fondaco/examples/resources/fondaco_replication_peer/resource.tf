# The EU site: its journal is pulled from the internal listener, and
# publishes for feeds homed there are forwarded to its public URL.
#
# The credential is a file the registry reads, never a value in state.
resource "registry_replication_peer" "eu" {
  name          = "eu"
  url           = "https://eu-internal.example.com:9443"
  public_url    = "https://eu.example.com"
  pull_interval = "2s"
  token_file    = "/etc/fondaco/peers/eu.token"
}
