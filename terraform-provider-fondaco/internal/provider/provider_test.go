package provider_test

import (
	"context"
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/fondaco-dev/fondaco/terraform-provider-fondaco/internal/client"
	"github.com/fondaco-dev/fondaco/terraform-provider-fondaco/internal/provider"
)

// The acceptance tests run against a real registry — the same binary and the
// same admin API a user would point Terraform at. There is no mock: the whole
// value of this provider is that the registry validates the document, and a
// mock would validate nothing while passing every test.
//
// conformance/terraform/run.sh brings up that registry, mints an
// administrator token and runs these with TF_ACC=1.

var testAccProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"registry": providerserver.NewProtocol6WithError(provider.New("test")()),
}

func testAccPreCheck(t *testing.T) {
	t.Helper()
	if os.Getenv(provider.EnvEndpoint) == "" {
		t.Skipf("%s is not set: no registry to test against", provider.EnvEndpoint)
	}
	if os.Getenv(provider.EnvToken) == "" {
		t.Skipf("%s is not set: no administrator credential", provider.EnvToken)
	}
}

// apiClient talks to the same registry the provider does, so a test can put
// the site into a state Terraform did not create — which is the only way to
// prove drift is really detected.
func apiClient(t *testing.T) *client.Client {
	t.Helper()
	c, err := client.New(client.Options{
		Endpoint: os.Getenv(provider.EnvEndpoint),
		Token:    os.Getenv(provider.EnvToken),
	})
	if err != nil {
		t.Fatalf("build API client: %v", err)
	}
	return c
}

func ctx() context.Context { return context.Background() }

// terraformState is the state a check function receives. Aliased so the
// checks below read as what they are rather than as a package path.
type terraformState = terraform.State

// checkSecretWorks proves the token in state is a credential and not just a
// string: it authenticates with it and asks the registry who that is.
func checkSecretWorks(t *testing.T, address string) resource.TestCheckFunc {
	t.Helper()
	return func(state *terraform.State) error {
		res, ok := state.RootModule().Resources[address]
		if !ok {
			t.Fatalf("%s is not in state", address)
		}
		secret := res.Primary.Attributes["secret"]
		name := res.Primary.Attributes["name"]
		if secret == "" {
			t.Fatalf("%s has no secret in state", address)
		}
		c, err := client.New(client.Options{
			Endpoint: os.Getenv(provider.EnvEndpoint),
			Token:    secret,
		})
		if err != nil {
			return err
		}
		var who client.WhoAmI
		if err := c.Get(context.Background(), "/whoami", &who); err != nil {
			t.Fatalf("the issued token does not authenticate: %v", err)
		}
		if who.Kind != "token" || who.Subject != name {
			t.Fatalf("the issued token authenticates as %s:%s, want token:%s",
				who.Kind, who.Subject, name)
		}
		return nil
	}
}
