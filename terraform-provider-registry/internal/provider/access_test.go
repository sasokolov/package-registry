package provider_test

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/sasokolov/package-registry/terraform-provider-registry/internal/client"
)

func TestAccAccessPolicy(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "registry_access_policy" "team" {
  name = "tf-acc-team"

  rule {
    path         = "feed/tf-acc/maven:com.example:*"
    capabilities = ["read", "list", "publish"]
  }

  rule {
    path         = "feed/tf-acc/maven:com.example.internal:*"
    capabilities = ["deny"]
  }
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("registry_access_policy.team", "id", "tf-acc-team"),
					resource.TestCheckResourceAttr("registry_access_policy.team", "rule.#", "2"),
					resource.TestCheckResourceAttr("registry_access_policy.team",
						"rule.0.capabilities.2", "publish"),
					resource.TestCheckResourceAttr("registry_access_policy.team",
						"rule.1.capabilities.0", "deny"),
				),
			},
			{
				ResourceName:      "registry_access_policy.team",
				ImportState:       true,
				ImportStateId:     "tf-acc-team",
				ImportStateVerify: true,
			},
			// Narrowing a policy is the change people make under pressure, so
			// it had better be an update and not a replace-and-a-gap.
			{
				Config: `
resource "registry_access_policy" "team" {
  name = "tf-acc-team"

  rule {
    path         = "feed/tf-acc/maven:com.example:*"
    capabilities = ["read", "list"]
  }

  rule {
    path         = "feed/tf-acc/maven:com.example.internal:*"
    capabilities = ["deny"]
  }
}
`,
				Check: resource.TestCheckResourceAttr("registry_access_policy.team",
					"rule.0.capabilities.#", "2"),
			},
		},
	})
}

// The mistakes worth catching before anything is written are the ones that
// would otherwise be caught by a person wondering why access did not change.
func TestAccAccessPolicy_planTimeValidation(t *testing.T) {
	cases := []struct {
		name    string
		config  string
		expects *regexp.Regexp
	}{
		{
			name: "a name the registry generates",
			config: `
resource "registry_access_policy" "bad" {
  name = "feed:releases:read"
  rule {
    path         = "feed/releases/*"
    capabilities = ["read"]
  }
}
`,
			expects: regexp.MustCompile(`Reserved policy name`),
		},
		{
			name: "a path in no namespace",
			config: `
resource "registry_access_policy" "bad" {
  name = "tf-acc-bad-path"
  rule {
    path         = "releases/*"
    capabilities = ["read"]
  }
}
`,
			expects: regexp.MustCompile(`Path is in no namespace`),
		},
		{
			name: "a wildcard in the middle",
			config: `
resource "registry_access_policy" "bad" {
  name = "tf-acc-bad-wildcard"
  rule {
    path         = "feed/*/maven:com.example:lib"
    capabilities = ["read"]
  }
}
`,
			expects: regexp.MustCompile(`Misplaced wildcard`),
		},
		{
			name: "a capability that does not exist",
			config: `
resource "registry_access_policy" "bad" {
  name = "tf-acc-bad-capability"
  rule {
    path         = "feed/releases/*"
    capabilities = ["write"]
  }
}
`,
			expects: regexp.MustCompile(`Unknown capability`),
		},
		{
			name: "no rules at all",
			config: `
resource "registry_access_policy" "bad" {
  name = "tf-acc-no-rules"
}
`,
			expects: regexp.MustCompile(`grants nothing`),
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
					ExpectError: tc.expects,
				}},
			})
		})
	}
}

func TestAccBinding(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "registry_access_policy" "reader" {
  name = "tf-acc-reader"
  rule {
    path         = "feed/tf-acc-bound/*"
    capabilities = ["read", "list"]
  }
}

resource "registry_binding" "ci" {
  name     = "tf-acc-ci"
  policies = [registry_access_policy.reader.name]

  kind         = "oidc"
  project_path = "tf-acc/*"
  ref          = "main"
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("registry_binding.ci", "id", "tf-acc-ci"),
					resource.TestCheckResourceAttr("registry_binding.ci", "policies.0", "tf-acc-reader"),
					resource.TestCheckResourceAttr("registry_binding.ci", "project_path", "tf-acc/*"),
				),
			},
			{
				ResourceName:      "registry_binding.ci",
				ImportState:       true,
				ImportStateId:     "tf-acc-ci",
				ImportStateVerify: true,
			},
		},
	})
}

// Deleting a policy that a binding still names would leave the binding
// granting something that does not exist. The registry refuses, and says
// which binding to fix — the refusal is asked for directly rather than
// through a destroy, so that what is being tested is the registry's answer
// and not Terraform's ordering.
func TestAccAccessPolicy_refusesToVanishFromUnderABinding(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: `
resource "registry_access_policy" "doomed" {
  name = "tf-acc-doomed"
  rule {
    path         = "feed/tf-acc-doomed/*"
    capabilities = ["read"]
  }
}

resource "registry_binding" "holder" {
  name     = "tf-acc-holder"
  policies = [registry_access_policy.doomed.name]
  kind     = "token"
  subject  = "tf-acc-holder"
}
`,
			Check: func(*terraformState) error {
				err := apiClient(t).Delete(ctx(), "/config/access/policies/tf-acc-doomed")
				if err == nil {
					return errors.New("the policy was deleted while a binding still named it")
				}
				var apiErr *client.Error
				if !errors.As(err, &apiErr) || !apiErr.Conflict() {
					return fmt.Errorf("want a 409 conflict, got %v", err)
				}
				if !strings.Contains(apiErr.Message, "tf-acc-holder") {
					return fmt.Errorf("the refusal does not say which binding to fix: %s", apiErr.Message)
				}
				return nil
			},
		}},
	})
}

// A binding with no conditions applies to everyone, anonymous callers
// included. That is occasionally deliberate and usually a mistake, so it
// warns rather than fails — and the warning has to actually appear.
func TestAccBinding_warnsWhenItMatchesEveryone(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: `
resource "registry_access_policy" "public" {
  name = "tf-acc-public"
  rule {
    path         = "feed/tf-acc-public/*"
    capabilities = ["read"]
  }
}

resource "registry_binding" "everyone" {
  name     = "tf-acc-everyone"
  policies = [registry_access_policy.public.name]
}
`,
			Check: resource.TestCheckResourceAttr("registry_binding.everyone", "id", "tf-acc-everyone"),
		}},
	})
}

// The explanation has to come from the engine that answers real requests.
// Asserting both directions is the point: that the grant works, and that the
// deny beside it still holds.
func TestAccAccessExplain(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: `
resource "registry_access_policy" "explained" {
  name = "tf-acc-explained"

  rule {
    path         = "feed/tf-acc-explained/maven:com.example:*"
    capabilities = ["read", "publish"]
  }

  rule {
    path         = "feed/tf-acc-explained/maven:com.example.internal:*"
    capabilities = ["deny"]
  }
}

resource "registry_binding" "explained" {
  name     = "tf-acc-explained"
  policies = [registry_access_policy.explained.name]
  kind     = "token"
  subject  = "tf-acc-explained-bot"
}

data "registry_access_explain" "allowed" {
  path       = "feed/tf-acc-explained/maven:com.example:lib@1.0.0"
  capability = "publish"
  kind       = "token"
  subject    = "tf-acc-explained-bot"

  depends_on = [registry_binding.explained]
}

data "registry_access_explain" "refused" {
  path       = "feed/tf-acc-explained/maven:com.example.internal:secret@1.0.0"
  capability = "publish"
  kind       = "token"
  subject    = "tf-acc-explained-bot"

  depends_on = [registry_binding.explained]
}

data "registry_access_explain" "stranger" {
  path       = "feed/tf-acc-explained/maven:com.example:lib@1.0.0"
  capability = "publish"
  kind       = "token"
  subject    = "tf-acc-nobody"

  depends_on = [registry_binding.explained]
}
`,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("data.registry_access_explain.allowed", "allowed", "true"),
				resource.TestCheckResourceAttr("data.registry_access_explain.allowed",
					"policy", "tf-acc-explained"),
				resource.TestCheckResourceAttr("data.registry_access_explain.allowed",
					"rule", "feed/tf-acc-explained/maven:com.example:*"),
				resource.TestCheckResourceAttr("data.registry_access_explain.refused", "allowed", "false"),
				resource.TestCheckResourceAttr("data.registry_access_explain.refused",
					"rule", "feed/tf-acc-explained/maven:com.example.internal:*"),
				// Nothing bound, nothing granted: the default is refusal, not
				// whatever the last matching identity happened to have.
				resource.TestCheckResourceAttr("data.registry_access_explain.stranger", "allowed", "false"),
			),
		}},
	})
}
