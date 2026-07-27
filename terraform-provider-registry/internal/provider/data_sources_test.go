package provider_test

import (
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// The data sources describe the live site, so what they must get right is
// that they describe *this* site — the one the provider is talking to — and
// that a feed Terraform just created is visible through them.
func TestAccDataSources(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: `
resource "registry_feed" "seen" {
  name      = "tf-acc-seen"
  format    = "maven"
  upstream  = "http://fake-upstream/maven"
  anonymous = true
}

data "registry_site" "this" {}

data "registry_feeds" "all" {
  depends_on = [registry_feed.seen]
}

data "registry_feed" "seen" {
  name       = registry_feed.seen.name
  depends_on = [registry_feed.seen]
}

data "registry_replication_status" "here" {}
`,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttrSet("data.registry_site.this", "site"),
				// The configuration version is a sha256 of the document, and
				// the site knowing it is what makes drift detectable at all.
				resource.TestMatchResourceAttr("data.registry_site.this", "config_version",
					regexp.MustCompile(`^[0-9a-f]{64}$`)),
				resource.TestCheckResourceAttr("data.registry_site.this", "database", "up"),

				resource.TestCheckResourceAttr("data.registry_feed.seen", "format", "maven"),
				resource.TestCheckResourceAttr("data.registry_feed.seen", "anonymous", "true"),

				// The feed just created appears in the listing, alongside the
				// ones the site was configured with by hand.
				resource.TestCheckTypeSetElemAttr("data.registry_feeds.all", "names.*", "tf-acc-seen"),
				resource.TestCheckTypeSetElemAttr("data.registry_feeds.all", "names.*", "hosted"),

				resource.TestCheckResourceAttrSet("data.registry_replication_status.here", "enabled"),
			),
		}},
	})
}
