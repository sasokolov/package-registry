package provider

import (
	"context"
	"net/url"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/fondaco-dev/fondaco/terraform-provider-fondaco/internal/client"
)

// NewOIDCIssuerResource returns the registry_oidc_issuer resource.
func NewOIDCIssuerResource() resource.Resource { return &oidcIssuerResource{} }

type oidcIssuerResource struct {
	client *client.Client
}

type oidcIssuerModel struct {
	ID                    types.String `tfsdk:"id"`
	Issuer                types.String `tfsdk:"issuer"`
	Audience              types.String `tfsdk:"audience"`
	JWKSURL               types.String `tfsdk:"jwks_url"`
	ClientID              types.String `tfsdk:"client_id"`
	ClientSecretEnv       types.String `tfsdk:"client_secret_env"`
	Scopes                types.List   `tfsdk:"scopes"`
	AuthorizationEndpoint types.String `tfsdk:"authorization_endpoint"`
	TokenEndpoint         types.String `tfsdk:"token_endpoint"`
}

func (r *oidcIssuerResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_oidc_issuer"
}

func (r *oidcIssuerResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "An OIDC issuer whose id_tokens this site accepts as identities — " +
			"typically a GitLab instance issuing CI job tokens. Adding one lets pipelines " +
			"authenticate without a static secret to leak.\n\n" +
			"Setting `client_id` additionally registers this registry as an OAuth client of " +
			"the issuer, which is what turns the console's sign-in from \"paste an id_token\" " +
			"into a button and a redirect. The two are independent: pipelines keep presenting " +
			"their own tokens either way.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The issuer URL, which is its identity.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"issuer": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Expected `iss` claim, e.g. `https://gitlab.com`. Trusting a " +
					"different issuer is a different trust decision, so changing it replaces the resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"audience": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Required `aud` claim. Tokens minted for anything else are refused.",
			},
			"jwks_url": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Explicit JWKS endpoint. Leave unset to let the registry discover " +
					"it from the issuer.",
			},
			"client_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "OAuth client ID for browser sign-in. Register the callback " +
					"`<registry>/ui/oidc/callback` at the issuer, as a public client using " +
					"PKCE. An id_token obtained this way is addressed to the client ID, and " +
					"the registry accepts it alongside `audience`.",
			},
			"client_secret_env": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Name of an environment variable in the registry's process " +
					"holding the client secret, for issuers that refuse to treat the registry as " +
					"a public client. The *name* is configuration; the secret is not, and never " +
					"enters the configuration document. A public client with PKCE needs none.",
			},
			"scopes": schema.ListAttribute{
				Optional:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Scopes requested at sign-in. Defaults to `[\"openid\"]`, " +
					"which is enough to learn who somebody is.",
			},
			"authorization_endpoint": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Overrides discovery, for an issuer that publishes no " +
					"discovery document or publishes a wrong one.",
			},
			"token_endpoint": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Overrides discovery, as `authorization_endpoint` does.",
			},
		},
	}
}

func (r *oidcIssuerResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = configureResource(req, resp)
}

func (r *oidcIssuerResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var model oidcIssuerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}
	validateHTTPURL(&resp.Diagnostics, pathOf("issuer"), model.Issuer)
	validateHTTPURL(&resp.Diagnostics, pathOf("jwks_url"), model.JWKSURL)
	validateHTTPURL(&resp.Diagnostics, pathOf("authorization_endpoint"), model.AuthorizationEndpoint)
	validateHTTPURL(&resp.Diagnostics, pathOf("token_endpoint"), model.TokenEndpoint)

	// A secret with no client to be the secret of is a line that looks like
	// it does something and does not.
	if !model.ClientSecretEnv.IsNull() && model.ClientSecretEnv.ValueString() != "" &&
		model.ClientID.ValueString() == "" && !model.ClientID.IsUnknown() {
		resp.Diagnostics.AddAttributeError(pathOf("client_secret_env"),
			"A client secret with no client",
			"client_secret_env only means something alongside client_id.")
	}
	// The value, not the name: pasting the secret here would put it in the
	// configuration document and in state, which is the thing this field
	// exists to avoid.
	if secret := model.ClientSecretEnv.ValueString(); strings.ContainsAny(secret, "=$ ") {
		resp.Diagnostics.AddAttributeError(pathOf("client_secret_env"),
			"This is the name of an environment variable, not the secret",
			"Set the secret in the registry's environment and name the variable here.")
	}
}

func (r *oidcIssuerResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan oidcIssuerModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.write(ctx, &plan, "trusting OIDC issuer "+plan.Issuer.ValueString(), &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *oidcIssuerResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state oidcIssuerModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	issuer := state.Issuer.ValueString()

	var found client.OIDCIssuer
	err := r.client.Get(ctx, client.Query("/config/oidc/issuer", "issuer", issuer), &found)
	if removedOutsideTerraform(ctx, err, resp) {
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read OIDC issuer "+issuer, err.Error())
		return
	}

	state.ID = types.StringValue(found.Issuer)
	state.Issuer = types.StringValue(found.Issuer)
	state.Audience = types.StringValue(found.Audience)
	state.JWKSURL = stringOrPrior(found.JWKSURL, state.JWKSURL)
	state.ClientID = stringOrPrior(found.ClientID, state.ClientID)
	state.ClientSecretEnv = stringOrPrior(found.ClientSecretEnv, state.ClientSecretEnv)
	state.AuthorizationEndpoint = stringOrPrior(found.AuthorizationEndpoint, state.AuthorizationEndpoint)
	state.TokenEndpoint = stringOrPrior(found.TokenEndpoint, state.TokenEndpoint)
	scopes, diags := stringsOrPrior(ctx, found.Scopes, state.Scopes)
	resp.Diagnostics.Append(diags...)
	state.Scopes = scopes
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *oidcIssuerResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan oidcIssuerModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.write(ctx, &plan, "updating OIDC issuer "+plan.Issuer.ValueString(), &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *oidcIssuerResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state oidcIssuerModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	issuer := state.Issuer.ValueString()
	err := r.client.Delete(ctx, client.Query("/config/oidc/issuer", "issuer", issuer))
	if err != nil && !isNotFound(err) {
		reportWriteError(&resp.Diagnostics, "removing OIDC issuer "+issuer, err)
	}
}

func (r *oidcIssuerResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("issuer"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

func (r *oidcIssuerResource) write(ctx context.Context, plan *oidcIssuerModel, action string, diags *diag.Diagnostics) {
	issuer := client.OIDCIssuer{
		Issuer:                plan.Issuer.ValueString(),
		Audience:              plan.Audience.ValueString(),
		JWKSURL:               plan.JWKSURL.ValueString(),
		ClientID:              plan.ClientID.ValueString(),
		ClientSecretEnv:       plan.ClientSecretEnv.ValueString(),
		Scopes:                stringsFrom(ctx, plan.Scopes, diags),
		AuthorizationEndpoint: plan.AuthorizationEndpoint.ValueString(),
		TokenEndpoint:         plan.TokenEndpoint.ValueString(),
	}
	if diags.HasError() {
		return
	}
	if _, err := r.client.Put(ctx, "/config/oidc/issuer", issuer); err != nil {
		reportWriteError(diags, action, err)
		return
	}
	plan.ID = types.StringValue(issuer.Issuer)
}

// validateHTTPURL rejects anything that is not an absolute http(s) URL, which
// is what the registry requires of both an issuer and a JWKS endpoint.
func validateHTTPURL(diags *diag.Diagnostics, at path.Path, value types.String) {
	raw := value.ValueString()
	if raw == "" || value.IsUnknown() {
		return
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		diags.AddAttributeError(at, "Not an absolute http(s) URL",
			"Expected something like https://gitlab.com, got "+raw)
	}
}
