package provider_test

import (
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/sasokolov/package-registry/terraform-provider-registry/internal/client"
)

func TestAccAdminBinding(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "registry_admin_binding" "infra" {
  pattern = "project:tf-acc-infra/*"
}
`,
				Check: resource.TestCheckResourceAttr(
					"registry_admin_binding.infra", "id", "project:tf-acc-infra/*"),
			},
			{
				ResourceName:      "registry_admin_binding.infra",
				ImportState:       true,
				ImportStateId:     "project:tf-acc-infra/*",
				ImportStateVerify: true,
			},
		},
	})
}

// Adding a binding must leave the administrator that Terraform itself is
// authenticating as in place — otherwise the second apply cannot run.
func TestAccAdminBinding_leavesOtherAdministratorsAlone(t *testing.T) {
	var before []string

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			var list client.AdminList
			if err := apiClient(t).Get(ctx(), "/config/admins", &list); err != nil {
				t.Fatalf("read admins: %v", err)
			}
			before = list.Admins
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: `
resource "registry_admin_binding" "extra" {
  pattern = "token:tf-acc-extra-*"
}
`,
			Check: func(*terraformState) error {
				var list client.AdminList
				if err := apiClient(t).Get(ctx(), "/config/admins", &list); err != nil {
					return err
				}
				for _, want := range before {
					if !contains(list.Admins, want) {
						t.Fatalf("administrator %q disappeared; list is now %v", want, list.Admins)
					}
				}
				if !contains(list.Admins, "token:tf-acc-extra-*") {
					t.Fatalf("the new binding is not in %v", list.Admins)
				}
				return nil
			},
		}},
	})
}

func TestAccOIDCIssuer(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "registry_oidc_issuer" "gitlab" {
  issuer   = "https://gitlab.tf-acc.example.com"
  audience = "registry"
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("registry_oidc_issuer.gitlab", "audience", "registry"),
					resource.TestCheckResourceAttr("registry_oidc_issuer.gitlab", "id",
						"https://gitlab.tf-acc.example.com"),
				),
			},
			{
				// Audience is an in-place change, not a replacement.
				Config: `
resource "registry_oidc_issuer" "gitlab" {
  issuer   = "https://gitlab.tf-acc.example.com"
  audience = "registry-eu"
  jwks_url = "https://gitlab.tf-acc.example.com/oauth/discovery/keys"
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("registry_oidc_issuer.gitlab", "audience", "registry-eu"),
					resource.TestCheckResourceAttr("registry_oidc_issuer.gitlab", "jwks_url",
						"https://gitlab.tf-acc.example.com/oauth/discovery/keys"),
				),
			},
			{
				ResourceName:      "registry_oidc_issuer.gitlab",
				ImportState:       true,
				ImportStateId:     "https://gitlab.tf-acc.example.com",
				ImportStateVerify: true,
			},
		},
	})
}

func TestAccOIDCIssuer_rejectsSomethingThatIsNotAURL(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: `
resource "registry_oidc_issuer" "bad" {
  issuer   = "gitlab.example.com"
  audience = "registry"
}
`,
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`Not an absolute http\(s\) URL`),
		}},
	})
}

func TestAccToken(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: `
resource "registry_token" "ci" {
  name = "tf-acc-ci"
}
`,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("registry_token.ci", "id", "tf-acc-ci"),
				resource.TestCheckResourceAttrSet("registry_token.ci", "secret"),
				// Eight characters of the hash, never the secret.
				resource.TestMatchResourceAttr("registry_token.ci", "hash_prefix",
					regexp.MustCompile(`^[0-9a-f]{8}$`)),
				// And the secret it handed back actually authenticates.
				checkSecretWorks(t, "registry_token.ci"),
			),
		}},
	})
}

func TestAccQuarantine(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "registry_quarantine" "cve" {
  feed       = "hosted"
  coordinate = "maven:com.example:tf-acc@1.0.0"
  detail     = "CVE-2026-0001, under investigation"
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("registry_quarantine.cve", "reason", "manual"),
					resource.TestCheckResourceAttr("registry_quarantine.cve", "id",
						"hosted/maven:com.example:tf-acc@1.0.0/manual"),
					resource.TestCheckResourceAttrSet("registry_quarantine.cve", "created_at"),
					func(*terraformState) error {
						var list client.QuarantineList
						if err := apiClient(t).Get(ctx(), "/quarantine", &list); err != nil {
							return err
						}
						for _, entry := range list.Quarantine {
							if entry.Coordinate == "maven:com.example:tf-acc@1.0.0" {
								return nil
							}
						}
						t.Fatalf("the coordinate is not blocked; quarantine is %v", list.Quarantine)
						return nil
					},
				),
			},
			{
				ResourceName:      "registry_quarantine.cve",
				ImportState:       true,
				ImportStateId:     "hosted/maven:com.example:tf-acc@1.0.0/manual",
				ImportStateVerify: true,
				// detail is not returned in a way import can reconstruct
				// before the first refresh; the refresh fills it in.
				ImportStateVerifyIgnore: []string{"detail"},
			},
		},
	})
}

// A conflict block is derived state. Declaring one would be declaring a
// symptom, so the provider refuses before anything is sent.
func TestAccQuarantine_refusesToDeclareAConflictBlock(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: `
resource "registry_quarantine" "derived" {
  feed       = "hosted"
  coordinate = "maven:com.example:tf-acc@2.0.0"
  reason     = "cross_site_conflict"
}
`,
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`This block cannot be declared`),
		}},
	})
}

func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}

// An issuer this registry is a registered client of. The browser fields are
// what turn the console's sign-in into a button, and they have to survive a
// round-trip through the configuration document like anything else.
func TestAccOIDCIssuer_browserSignIn(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "registry_oidc_issuer" "sso" {
  issuer    = "https://tf-acc-sso.example.com"
  audience  = "package-registry"
  client_id = "registry-console"
  scopes    = ["openid", "email"]

  authorization_endpoint = "https://tf-acc-sso.example.com/authorize"
  token_endpoint         = "https://tf-acc-sso.example.com/token"
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("registry_oidc_issuer.sso", "client_id", "registry-console"),
					resource.TestCheckResourceAttr("registry_oidc_issuer.sso", "scopes.1", "email"),
					resource.TestCheckResourceAttr("registry_oidc_issuer.sso",
						"token_endpoint", "https://tf-acc-sso.example.com/token"),
				),
			},
			{
				ResourceName:      "registry_oidc_issuer.sso",
				ImportState:       true,
				ImportStateId:     "https://tf-acc-sso.example.com",
				ImportStateVerify: true,
			},
		},
	})
}

// The secret is named, never written. Catching the paste at plan time is the
// difference between a secret in one person's shell history and a secret in
// the configuration document, in state, and in git.
func TestAccOIDCIssuer_refusesTheSecretItself(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "registry_oidc_issuer" "bad" {
  issuer            = "https://tf-acc-secret.example.com"
  audience          = "package-registry"
  client_id         = "registry-console"
  client_secret_env = "hunter2 $ecret"
}
`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`name of an environment variable`),
			},
			{
				Config: `
resource "registry_oidc_issuer" "bad" {
  issuer            = "https://tf-acc-secret.example.com"
  audience          = "package-registry"
  client_secret_env = "REGISTRY_OIDC_CLIENT_SECRET"
}
`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`client secret with no client`),
			},
		},
	})
}
