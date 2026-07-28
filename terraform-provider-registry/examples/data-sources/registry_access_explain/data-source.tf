# Ask the registry what it would decide, rather than reasoning about the
# rules yourself. The answer comes from the same engine that answers real
# requests, so it cannot drift from it.

# Would the CI job be allowed to publish where we think it is?
data "registry_access_explain" "acme_ci_publish" {
  path       = "feed/releases/maven:com.acme:widget@1.0.0"
  capability = "publish"

  kind         = "oidc"
  issuer       = "https://gitlab.example.com"
  subject      = "project_path:acme/widget:ref:main"
  project_path = "acme/widget"
  ref          = "main"
}

# And — the half people forget to check — is it still refused where it
# should be? An access change that quietly widens is not visible in a plan;
# asserting the refusal makes it visible.
data "registry_access_explain" "acme_ci_internal" {
  path       = "feed/releases/maven:com.acme.internal:secret@1.0.0"
  capability = "publish"

  kind         = "oidc"
  issuer       = "https://gitlab.example.com"
  project_path = "acme/widget"
  ref          = "main"
}

check "access_is_what_we_meant" {
  assert {
    condition     = data.registry_access_explain.acme_ci_publish.allowed
    error_message = "acme CI can no longer publish its own artifacts: ${data.registry_access_explain.acme_ci_publish.reason}"
  }

  assert {
    condition     = !data.registry_access_explain.acme_ci_internal.allowed
    error_message = "acme CI gained publish on the internal namespace via ${data.registry_access_explain.acme_ci_internal.policy}"
  }
}

# Which rule decided, for the runbook entry that explains a refusal.
output "why" {
  value = "${data.registry_access_explain.acme_ci_publish.policy} at ${data.registry_access_explain.acme_ci_publish.rule}"
}
