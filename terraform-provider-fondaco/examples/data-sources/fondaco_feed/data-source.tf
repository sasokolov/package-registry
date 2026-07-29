# Read a feed the site already has, whether or not Terraform manages it.
data "fondaco_feed" "central" {
  name = "central"
}

output "central_upstream" {
  value = data.fondaco_feed.central.upstream
}
