# What a team may do with its own namespace, and nothing else. Read and list
# are separate capabilities: a group that may resolve an artifact it already
# knows the coordinate of is not necessarily one that may enumerate what
# exists.
resource "fondaco_access_policy" "team_acme" {
  name = "team-acme"

  rule {
    path         = "feed/releases/maven:com.acme:*"
    capabilities = ["read", "list", "publish"]
  }

  # Deliberate exception to the line above. A deny beats every other
  # capability at the same specificity, and this rule is more specific, so
  # it holds even though the broader rule grants publish.
  rule {
    path         = "feed/releases/maven:com.acme.internal:*"
    capabilities = ["deny"]
  }
}

# Read-only across every feed, for a dashboard or an auditor.
resource "fondaco_access_policy" "observer" {
  name = "observer"

  rule {
    path         = "feed/*"
    capabilities = ["read", "list"]
  }

  rule {
    path         = "sys/status"
    capabilities = ["read"]
  }
}

# Operating the site without being able to change what it serves: the
# on-call rotation can release a quarantined package but cannot rewrite
# feeds or mint tokens.
resource "fondaco_access_policy" "oncall" {
  name = "oncall"

  rule {
    path         = "sys/quarantine"
    capabilities = ["read", "update"]
  }

  rule {
    path         = "sys/replication"
    capabilities = ["read"]
  }

  rule {
    path         = "sys/conflicts"
    capabilities = ["read", "update"]
  }
}
