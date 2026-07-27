package provider_test

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/sasokolov/package-registry/terraform-provider-registry/internal/client"
)

// A feed goes through its whole life: declared, changed, imported, destroyed —
// and the plan is empty in between, which is the only proof that what the
// provider wrote and what it reads back are the same thing.
func TestAccFeedResource(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "registry_feed" "acc" {
  name      = "tf-acc-proxy"
  format    = "maven"
  upstream  = "http://fake-upstream/maven"
  anonymous = true
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("registry_feed.acc", "id", "tf-acc-proxy"),
					resource.TestCheckResourceAttr("registry_feed.acc", "format", "maven"),
					resource.TestCheckResourceAttr("registry_feed.acc", "anonymous", "true"),
					resource.TestCheckResourceAttr("registry_feed.acc", "upstream", "http://fake-upstream/maven"),
				),
			},
			{
				// Everything the schema can express, on one feed.
				Config: `
resource "registry_feed" "acc" {
  name         = "tf-acc-proxy"
  format       = "maven"
  upstream     = "http://fake-upstream/maven"
  anonymous    = true
  hosted       = true
  publishers   = ["token:ci-*"]
  upstream_rps = 12.5
  redirect     = true
  redirect_ttl = "20m"

  policy {
    name   = "allowlist"
    config = jsonencode({ allow = ["com.example:liba"] })
  }
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("registry_feed.acc", "hosted", "true"),
					resource.TestCheckResourceAttr("registry_feed.acc", "publishers.0", "token:ci-*"),
					resource.TestCheckResourceAttr("registry_feed.acc", "upstream_rps", "12.5"),
					// The registry normalises "20m" to "20m0s"; the operator's
					// spelling has to survive, or every plan shows a change.
					resource.TestCheckResourceAttr("registry_feed.acc", "redirect_ttl", "20m"),
					resource.TestCheckResourceAttr("registry_feed.acc", "policy.0.name", "allowlist"),
				),
			},
			{
				ResourceName:      "registry_feed.acc",
				ImportState:       true,
				ImportStateId:     "tf-acc-proxy",
				ImportStateVerify: true,
				// Import knows the name; everything else comes from the site.
				// redirect_ttl is the one field the registry rewrites, so a
				// fresh import legitimately reads "20m0s" where the config
				// says "20m".
				ImportStateVerifyIgnore: []string{"redirect_ttl"},
			},
		},
	})
}

// A change made outside Terraform has to show up as a change. This is the
// property that makes "configuration as code" mean anything.
func TestAccFeedResource_driftIsDetected(t *testing.T) {
	const name = "tf-acc-drift"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "registry_feed" "drift" {
  name      = %q
  format    = "maven"
  upstream  = "http://fake-upstream/maven"
  anonymous = true
}
`, name),
			},
			{
				// Someone edits the feed through the API. A refresh must
				// notice, and the next plan must want to put it back.
				PreConfig: func() {
					c := apiClient(t)
					_, err := c.Put(ctx(), "/config/feeds/"+name, client.Feed{
						Name:      name,
						Format:    "maven",
						Upstream:  "http://fake-upstream/maven",
						Anonymous: false,
					})
					if err != nil {
						t.Fatalf("out-of-band edit: %v", err)
					}
				},
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
				Check: resource.TestCheckResourceAttr(
					"registry_feed.drift", "anonymous", "false"),
			},
			{
				// And applying puts it back.
				Config: fmt.Sprintf(`
resource "registry_feed" "drift" {
  name      = %q
  format    = "maven"
  upstream  = "http://fake-upstream/maven"
  anonymous = true
}
`, name),
				Check: resource.TestCheckResourceAttr("registry_feed.drift", "anonymous", "true"),
			},
		},
	})
}

// A feed deleted through the API is gone, not corrupt state: the next plan
// offers to create it again.
func TestAccFeedResource_deletedOutsideTerraform(t *testing.T) {
	const name = "tf-acc-vanish"
	config := fmt.Sprintf(`
resource "registry_feed" "vanish" {
  name      = %q
  format    = "maven"
  upstream  = "http://fake-upstream/maven"
  anonymous = true
}
`, name)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig: func() {
					if err := apiClient(t).Delete(ctx(), "/config/feeds/"+name); err != nil {
						t.Fatalf("out-of-band delete: %v", err)
					}
				},
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
			},
			{Config: config},
		},
	})
}

// Plan-time validation: the mistakes worth catching before an apply starts.
func TestAccFeedResource_planTimeValidation(t *testing.T) {
	cases := []struct {
		name   string
		config string
		expect *regexp.Regexp
	}{
		{
			name: "an uppercase name is not a URL path segment",
			config: `
resource "registry_feed" "bad" {
  name     = "Maven-Central"
  format   = "maven"
  upstream = "http://fake-upstream/maven"
}`,
			expect: regexp.MustCompile(`Invalid feed name`),
		},
		{
			name: "a feed cannot be called ui",
			config: `
resource "registry_feed" "bad" {
  name     = "ui"
  format   = "maven"
  upstream = "http://fake-upstream/maven"
}`,
			expect: regexp.MustCompile(`Reserved feed name`),
		},
		{
			name: "a feed that neither proxies nor hosts serves nothing",
			config: `
resource "registry_feed" "bad" {
  name   = "tf-acc-empty"
  format = "maven"
}`,
			expect: regexp.MustCompile(`Feed serves nothing`),
		},
		{
			name: "publishers without hosting have nothing to publish to",
			config: `
resource "registry_feed" "bad" {
  name       = "tf-acc-pub"
  format     = "maven"
  upstream   = "http://fake-upstream/maven"
  publishers = ["token:ci-*"]
}`,
			expect: regexp.MustCompile(`Publishers require a hosted feed`),
		},
		{
			name: "an unknown replication mode is refused",
			config: `
resource "registry_feed" "bad" {
  name             = "tf-acc-mode"
  format           = "maven"
  upstream         = "http://fake-upstream/maven"
  replication_mode = "instant"
}`,
			expect: regexp.MustCompile(`Unsupported replication mode`),
		},
		{
			name: "forward: needs a site",
			config: `
resource "registry_feed" "bad" {
  name           = "tf-acc-fwd"
  format         = "maven"
  upstream       = "http://fake-upstream/maven"
  publish_policy = "forward:"
}`,
			expect: regexp.MustCompile(`Missing forward target`),
		},
		{
			name: "a bad duration is caught before it reaches the document",
			config: `
resource "registry_feed" "bad" {
  name         = "tf-acc-ttl"
  format       = "maven"
  upstream     = "http://fake-upstream/maven"
  redirect_ttl = "20 minutes"
}`,
			expect: regexp.MustCompile(`Invalid duration`),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resource.Test(t, resource.TestCase{
				PreCheck:                 func() { testAccPreCheck(t) },
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{{
					Config:      tc.config,
					PlanOnly:    true,
					ExpectError: tc.expect,
				}},
			})
		})
	}
}

// The registry has rules the provider deliberately does not duplicate,
// because they depend on the whole document. They must still be reported
// clearly rather than as an opaque failure.
func TestAccFeedResource_registryRefusesWhatOnlyItCanSee(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: `
resource "registry_feed" "homed_nowhere" {
  name           = "tf-acc-homed"
  format         = "maven"
  hosted         = true
  publish_policy = "forward:no-such-site"
}
`,
			ExpectError: regexp.MustCompile(`(?s)refused.*no peer with that name`),
		}},
	})
}
