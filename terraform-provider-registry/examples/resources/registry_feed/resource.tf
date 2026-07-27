# A proxy feed: cache Maven Central, open to everyone inside the network.
resource "registry_feed" "central" {
  name      = "central"
  format    = "maven"
  upstream  = "https://repo1.maven.org/maven2"
  anonymous = true

  # Only these coordinates may pass, whatever the upstream offers.
  policy {
    name = "allowlist"
    config = jsonencode({
      allow = ["com.example:*", "org.slf4j:*"]
    })
  }
}

# A hosted feed: our own releases, published by CI only, immutable once
# published.
resource "registry_feed" "releases" {
  name       = "releases"
  format     = "maven"
  hosted     = true
  anonymous  = true
  publishers = ["token:ci-*", "project:platform/*"]

  # Serve artifacts as pre-signed redirects instead of streaming them.
  redirect     = true
  redirect_ttl = "15m"
}

# A feed homed at another site: reads are served here, writes are forwarded to
# where the feed lives, and its blobs arrive before anyone asks for them.
resource "registry_feed" "eu_releases" {
  name             = "eu-releases"
  format           = "npm"
  hosted           = true
  anonymous        = true
  publishers       = ["project:eu/*"]
  publish_policy   = "forward:eu"
  replication_mode = "eager"
  peer_fallback    = true
}

# A group: one URL for clients, over everything the site holds. Order is
# precedence — the hosted feed comes first, so an internally published
# coordinate wins over a public one of the same name. The documents that list
# versions are merged across members, so neither hides the other.
#
# A group cannot proxy or host itself; it is a view.
resource "registry_feed" "maven_public" {
  name      = "maven-public"
  format    = "maven"
  anonymous = true
  members = [
    registry_feed.releases.name,
    registry_feed.central.name,
  ]
}
