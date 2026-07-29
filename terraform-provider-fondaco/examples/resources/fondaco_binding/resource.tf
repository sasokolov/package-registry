# A policy grants nothing until a binding says whose it is.
#
# Every condition set on a binding must hold, and conditions left unset are
# not checked — so a binding with no conditions applies to everyone,
# anonymous callers included. That is occasionally what you want (public
# read of a proxy feed) and usually not.

# GitLab CI jobs in one group, on the default branch only.
resource "fondaco_binding" "acme_ci" {
  name     = "acme-ci"
  policies = [fondaco_access_policy.team_acme.name]

  kind         = "oidc"
  issuer       = "https://gitlab.example.com"
  project_path = "acme/*"
  ref          = "main"
}

# A named static token, for the release job that predates OIDC.
resource "fondaco_binding" "release_bot" {
  name     = "release-bot"
  policies = [fondaco_access_policy.team_acme.name]

  kind    = "token"
  subject = "release-bot"
}

# Anyone who authenticated at all, by whatever means. Useful for the
# read-only policies you are happy for every employee to have and unwilling
# to hand to the internet.
resource "fondaco_binding" "staff" {
  name          = "staff"
  policies      = [fondaco_access_policy.observer.name]
  authenticated = true
}

# Two policies on one binding: the union of what they grant, with any deny
# in either still winning at its own specificity.
resource "fondaco_binding" "oncall" {
  name     = "oncall"
  policies = [fondaco_access_policy.oncall.name, fondaco_access_policy.observer.name]

  kind         = "oidc"
  issuer       = "https://gitlab.example.com"
  project_path = "infra/oncall"
}
