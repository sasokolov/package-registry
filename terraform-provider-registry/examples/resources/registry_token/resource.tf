# The secret is returned once, at creation, and lands in Terraform state.
# Treat the state as you would treat the token: encrypted backend, or pipe
# the value straight into a secret manager and never read it back.
resource "registry_token" "ci" {
  name = "ci-frontend"
}

# Grant it publishing rights by naming it in a feed's publishers.
resource "registry_feed" "frontend" {
  name       = "frontend"
  format     = "npm"
  hosted     = true
  anonymous  = true
  publishers = ["token:${registry_token.ci.name}"]
}

output "ci_token" {
  value     = registry_token.ci.secret
  sensitive = true
}
