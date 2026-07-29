# Anyone in the infra group's CI may administer this site.
resource "registry_admin_binding" "infra" {
  pattern = "project:infra/*"
}

# As may a named operator token.
resource "registry_admin_binding" "oncall" {
  pattern = "token:ops-oncall"
}
