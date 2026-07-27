package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/sasokolov/package-registry/terraform-provider-registry/internal/client"
)

// NewTokenResource returns the registry_token resource.
func NewTokenResource() resource.Resource { return &tokenResource{} }

type tokenResource struct {
	client *client.Client
}

type tokenModel struct {
	ID         types.String `tfsdk:"id"`
	Name       types.String `tfsdk:"name"`
	Secret     types.String `tfsdk:"secret"`
	HashPrefix types.String `tfsdk:"hash_prefix"`
	CreatedAt  types.String `tfsdk:"created_at"`
}

func (r *tokenResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_token"
}

func (r *tokenResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A static registry token.\n\n" +
			"The registry stores only a hash, so the secret exists exactly once: in the response " +
			"that issues it. That response is written to Terraform state, which is therefore as " +
			"sensitive as the token itself — use a state backend that is encrypted, or feed the " +
			"secret straight into a secret manager and never read it again.\n\n" +
			"For the same reason this resource cannot be imported: an existing token's secret is " +
			"not recoverable from anywhere. Destroying it revokes it everywhere, including at " +
			"every replicated site.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The token name, which is its identity.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Token name. It appears in audit lines and in feed publisher " +
					"patterns as `token:<name>`. Renaming issues a new token and revokes the old one.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"secret": schema.StringAttribute{
				Computed:            true,
				Sensitive:           true,
				MarkdownDescription: "The token itself, returned once when it is issued.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"hash_prefix": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "First eight characters of the stored hash — enough to correlate " +
					"with an audit line, useless as a credential.",
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "When the registry issued it.",
			},
		},
	}
}

func (r *tokenResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = configureResource(req, resp)
}

func (r *tokenResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan tokenModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := plan.Name.ValueString()

	var issued client.IssuedToken
	if err := r.client.Post(ctx, "/tokens", map[string]string{"name": name}, &issued); err != nil {
		reportWriteError(&resp.Diagnostics, "issuing token "+name, err)
		return
	}

	plan.ID = types.StringValue(name)
	plan.Secret = types.StringValue(issued.Secret)
	// The listing is the only place the hash prefix and timestamp come from,
	// and it costs one request to have them in state from the start.
	if found, ok := r.lookup(ctx, name); ok {
		plan.HashPrefix = types.StringValue(found.HashPrefix)
		plan.CreatedAt = types.StringValue(found.CreatedAt)
	} else {
		// The token was issued — the secret above is real — but the listing
		// that describes it could not be read. Say so instead of inventing
		// values that the next refresh would silently correct.
		plan.HashPrefix = types.StringNull()
		plan.CreatedAt = types.StringNull()
		resp.Diagnostics.AddWarning(
			"Issued token "+name+", but could not read it back",
			"The secret in state is valid. hash_prefix and created_at will be filled in "+
				"by the next refresh.",
		)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read reports the token gone when it no longer exists or has been revoked
// outside Terraform. A revoked token is not a token any more: leaving it in
// state would mean a pipeline keeps a credential that stopped working.
func (r *tokenResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state tokenModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()

	found, ok := r.lookup(ctx, name)
	if !ok || found.RevokedAt != nil {
		resp.State.RemoveResource(ctx)
		return
	}
	state.ID = types.StringValue(name)
	state.HashPrefix = types.StringValue(found.HashPrefix)
	state.CreatedAt = types.StringValue(found.CreatedAt)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update cannot happen: every attribute is either the identity or computed,
// and the identity requires replacement.
func (r *tokenResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan tokenModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *tokenResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state tokenModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()
	err := r.client.Delete(ctx, "/tokens/"+name)
	if err != nil && !isNotFound(err) {
		reportWriteError(&resp.Diagnostics, "revoking token "+name, err)
	}
}

// lookup finds one token in the listing. There is no per-token endpoint
// because there is nothing per-token to return that the listing does not
// already carry — and a secret is not among it.
func (r *tokenResource) lookup(ctx context.Context, name string) (client.Token, bool) {
	var list client.TokenList
	if err := r.client.Get(ctx, "/tokens", &list); err != nil {
		return client.Token{}, false
	}
	for _, token := range list.Tokens {
		if token.Name == name {
			return token, true
		}
	}
	return client.Token{}, false
}
