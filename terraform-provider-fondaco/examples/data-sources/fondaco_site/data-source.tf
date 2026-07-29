data "registry_site" "this" {}

# Useful for keying configuration on which site this is, instead of
# hardcoding it in every workspace.
output "site" {
  value = data.registry_site.this.site
}

output "config_version" {
  value = data.registry_site.this.config_version
}
