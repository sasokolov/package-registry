# A policy grants nothing until a binding says whose it is.
#
# Every condition set on a binding must hold, and conditions left unset are
# not checked — so a binding with no conditions applies to everyone,
# anonymous callers included. That is occasionally what you want (public
# read of a proxy feed) and usually not.

# GitLab CI jobs in one group, on the default branch only.
resource "registry_binding" "acme_ci" {
  name     = "acme-ci"
  policies = [registry_access_policy.team_acme.name]

  kind         = "oidc"
  issuer       = "https://gitlab.example.com"
  project_path = "acme/*"
  ref          = "main"
}

# A named static token, for the release job that predates OIDC.
resource "registry_binding" "release_bot" {
  name     = "release-bot"
  policies = [registry_access_policy.team_acme.name]

  kind    = "token"
  subject = "release-bot"
}

# Anyone who authenticated at all, by whatever means. Useful for the
# read-only policies you are happy for every employee to have and unwilling
# to hand to the internet.
resource "registry_binding" "staff" {
  name          = "staff"
  policies      = [registry_access_policy.observer.name]
  authenticated = true
}

# Two policies on one binding: the union of what they grant, with any deny
# in either still winning at its own specificity.
resource "registry_binding" "oncall" {
  name     = "oncall"
  policies = [registry_access_policy.oncall.name, registry_access_policy.observer.name]

  kind         = "oidc"
  issuer       = "https://gitlab.example.com"
  project_path = "infra/oncall"
}
