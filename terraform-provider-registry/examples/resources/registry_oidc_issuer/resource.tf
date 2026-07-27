# Accept GitLab CI id_tokens, so pipelines authenticate without a static
# secret that could leak.
resource "registry_oidc_issuer" "gitlab" {
  issuer   = "https://gitlab.example.com"
  audience = "package-registry"
}
