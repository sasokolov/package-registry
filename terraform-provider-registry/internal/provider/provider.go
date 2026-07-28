// Package provider implements the Terraform provider for the package
// registry.
//
// The registry's configuration is one declarative YAML document that already
// lives outside its database and is already validated as a whole before it is
// stored. That is exactly the shape Terraform wants, so this provider does
// not invent a second model of it: each resource maps to one endpoint that
// replaces one part of that document, and the registry stays the only thing
// that decides whether the result is valid.
package provider

import (
	"context"
	"os"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/sasokolov/package-registry/terraform-provider-registry/internal/client"
)

// Environment variables that stand in for provider arguments, so a token
// never has to be written into a .tf file to make the provider work.
const (
	EnvEndpoint = "REGISTRY_ENDPOINT"
	EnvToken    = "REGISTRY_TOKEN"
)

// New returns the provider, versioned by the build.
func New(version string) func() provider.Provider {
	return func() provider.Provider { return &registryProvider{version: version} }
}

type registryProvider struct {
	version string
}

type providerModel struct {
	Endpoint types.String `tfsdk:"endpoint"`
	Token    types.String `tfsdk:"token"`
	Insecure types.Bool   `tfsdk:"insecure"`
	Timeout  types.String `tfsdk:"timeout"`
}

func (p *registryProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "registry"
	resp.Version = p.version
}

func (p *registryProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a self-hosted package registry: feeds and their upstream " +
			"connectors, who may administer and publish, trusted OIDC issuers, replication " +
			"peers, static tokens and quarantine.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Base URL of the registry, e.g. `https://registry.example.com`. " +
					"Defaults to the `" + EnvEndpoint + "` environment variable.",
			},
			"token": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "A registry token or OIDC id_token whose identity matches one " +
					"of the site's `admins` patterns. Defaults to the `" + EnvToken + "` " +
					"environment variable, which is where it belongs: a credential in a `.tf` " +
					"file is a credential in version control.",
			},
			"insecure": schema.BoolAttribute{
				Optional:            true,
				MarkdownDescription: "Skip TLS verification. Development only.",
			},
			"timeout": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Per-request timeout, e.g. `30s`. Defaults to `30s`.",
			},
		},
	}
}

func (p *registryProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var config providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	endpoint := config.Endpoint.ValueString()
	if endpoint == "" {
		endpoint = os.Getenv(EnvEndpoint)
	}
	if endpoint == "" {
		resp.Diagnostics.AddAttributeError(
			pathRoot("endpoint"),
			"Registry endpoint is not set",
			"Set the provider's endpoint argument or the "+EnvEndpoint+" environment variable.",
		)
		return
	}

	token := config.Token.ValueString()
	if token == "" {
		token = os.Getenv(EnvToken)
	}

	var timeout time.Duration
	if raw := config.Timeout.ValueString(); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			resp.Diagnostics.AddAttributeError(
				pathRoot("timeout"),
				"Invalid timeout",
				"Expected a Go duration such as \"30s\": "+err.Error(),
			)
			return
		}
		timeout = parsed
	}

	c, err := client.New(client.Options{
		Endpoint: endpoint,
		Token:    token,
		Insecure: config.Insecure.ValueBool(),
		Timeout:  timeout,
	})
	if err != nil {
		resp.Diagnostics.AddError("Cannot reach the registry", err.Error())
		return
	}

	// Fail here rather than in the first resource: an unusable credential is
	// a provider problem, and saying so once is clearer than saying it once
	// per resource.
	var who client.WhoAmI
	if err := c.Get(ctx, "/whoami", &who); err != nil {
		resp.Diagnostics.AddError(
			"Cannot authenticate to the registry",
			"GET "+endpoint+client.APIPath+"/whoami failed: "+err.Error(),
		)
		return
	}
	if !who.Admin {
		resp.Diagnostics.AddWarning(
			"This identity is not an administrator",
			"The registry recognises this credential as "+who.Kind+":"+who.Subject+
				", but it does not match the site's admins patterns. Data sources will work; "+
				"anything that changes configuration will be refused with 403.",
		)
	}

	resp.ResourceData = c
	resp.DataSourceData = c
}

func (p *registryProvider) Resources(context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewFeedResource,
		NewAdminBindingResource,
		NewOIDCIssuerResource,
		NewReplicationPeerResource,
		NewTokenResource,
		NewQuarantineResource,
		NewAccessPolicyResource,
		NewBindingResource,
	}
}

func (p *registryProvider) DataSources(context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewSiteDataSource,
		NewFeedDataSource,
		NewFeedsDataSource,
		NewReplicationStatusDataSource,
		NewAccessDataSource,
	}
}
