data "fondaco_feeds" "all" {}

# What exists on the site but not in this configuration — the feeds someone
# added by hand.
output "unmanaged_feeds" {
  value = setsubtract(
    toset(data.fondaco_feeds.all.names),
    toset([for f in fondaco_feed.managed : f.name]),
  )
}
