# A GitLab instance whose CI jobs authenticate with their own id_tokens. No
# static secret exists to leak, and nothing here lets a person sign in — a
# pipeline brings its own token.
resource "registry_oidc_issuer" "gitlab" {
  issuer   = "https://gitlab.example.com"
  audience = "fondaco"
}

# The same registry, additionally registered as an OAuth client of the
# company's identity provider. That is what turns the console's sign-in from
# "paste an id_token" into a button: the browser is redirected, comes back
# with a code, and the registry exchanges it.
#
# Register the callback https://registry.example.com/ui/oidc/callback at the
# provider, as a public client using PKCE.
resource "registry_oidc_issuer" "sso" {
  issuer    = "https://sso.example.com"
  audience  = "fondaco"
  client_id = "registry-console"
  scopes    = ["openid"]
}

# An issuer that publishes no discovery document, and insists on treating
# clients as confidential. The secret itself stays in the registry's
# environment; only the variable's name is configuration.
resource "registry_oidc_issuer" "legacy" {
  issuer   = "https://legacy-idp.example.com"
  audience = "fondaco"

  client_id         = "registry-console"
  client_secret_env = "REGISTRY_OIDC_CLIENT_SECRET"

  authorization_endpoint = "https://legacy-idp.example.com/oauth2/authorize"
  token_endpoint         = "https://legacy-idp.example.com/oauth2/token"
  jwks_url               = "https://legacy-idp.example.com/oauth2/keys"
}
