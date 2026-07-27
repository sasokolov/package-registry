# A realistic set of feeds, connectors and permissions, applied from nothing.
# This is the acceptance criterion of the Terraform phase in one file: it must
# apply cleanly, re-plan empty, and notice an edit made behind its back.

terraform {
  required_providers {
    registry = {
      source = "registry.local/sasokolov/registry"
    }
  }
}

provider "registry" {}

# --- proxy feeds -------------------------------------------------------------

resource "registry_feed" "central" {
  name      = "tf-e2e-central"
  format    = "maven"
  upstream  = "http://fake-upstream/maven"
  anonymous = true

  policy {
    name = "allowlist"
    config = jsonencode({
      allow = ["com.example:liba"]
    })
  }
}

resource "registry_feed" "npm" {
  name         = "tf-e2e-npm"
  format       = "npm"
  upstream     = "http://fake-upstream/npm"
  anonymous    = true
  upstream_rps = 20
}

# --- hosted feed with permissions --------------------------------------------

resource "registry_token" "ci" {
  name = "ci-tf-e2e"
}

resource "registry_feed" "releases" {
  name       = "tf-e2e-releases"
  format     = "maven"
  hosted     = true
  anonymous  = true
  publishers = ["token:${registry_token.ci.name}"]
}

# --- who may administer, and who may authenticate ----------------------------

resource "registry_admin_binding" "platform" {
  pattern = "project:tf-e2e-platform/*"
}

resource "registry_oidc_issuer" "gitlab" {
  issuer   = "https://gitlab.tf-e2e.example.com"
  audience = "package-registry"
}

# --- what the site says about itself -----------------------------------------

data "registry_site" "this" {}

output "site" {
  value = data.registry_site.this.site
}

# The secret is an output so the end-to-end check can prove the token this
# configuration issued actually publishes to the feed that names it.
output "ci_token" {
  value     = registry_token.ci.secret
  sensitive = true
}

output "feed_names" {
  value = sort([
    registry_feed.central.name,
    registry_feed.npm.name,
    registry_feed.releases.name,
  ])
}
