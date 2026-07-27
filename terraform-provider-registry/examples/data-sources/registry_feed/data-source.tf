# Read a feed the site already has, whether or not Terraform manages it.
data "registry_feed" "central" {
  name = "central"
}

output "central_upstream" {
  value = data.registry_feed.central.upstream
}
