package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/sasokolov/package-registry/terraform-provider-registry/internal/client"
)

// NewBindingResource returns the registry_binding resource.
func NewBindingResource() resource.Resource { return &bindingResource{} }

type bindingResource struct {
	client *client.Client
}

type bindingModel struct {
	ID            types.String `tfsdk:"id"`
	Name          types.String `tfsdk:"name"`
	Policies      types.List   `tfsdk:"policies"`
	Kind          types.String `tfsdk:"kind"`
	Issuer        types.String `tfsdk:"issuer"`
	Subject       types.String `tfsdk:"subject"`
	ProjectPath   types.String `tfsdk:"project_path"`
	Ref           types.String `tfsdk:"ref"`
	Authenticated types.Bool   `tfsdk:"authenticated"`
}

func (r *bindingResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_binding"
}

func (r *bindingResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Attaches access policies to the identities a match selects.\n\n" +
			"A policy says what may be done; a binding says whose identity it applies " +
			"to, in terms of what authentication established — the kind of credential, " +
			"the issuer that vouched for it, the token name or subject, and the CI " +
			"claims. The policy itself knows nothing about who holds it, so one policy " +
			"serves many teams.\n\n" +
			"Every condition is a glob with an optional trailing `*`, and an omitted " +
			"condition is not a condition. A binding with no conditions therefore " +
			"applies to everyone, anonymous callers included — which is occasionally " +
			"what you want and usually not.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The binding name, which is its identity.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Binding name, so it can be discussed and changed.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"policies": schema.ListAttribute{
				Required:    true,
				ElementType: types.StringType,
				MarkdownDescription: "The policies this binding attaches. Each must exist, " +
					"or the configuration will not load.",
			},
			"kind": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "`token`, `oidc` or `anonymous`.",
			},
			"issuer": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "The OIDC issuer that vouched for the identity. Worth setting " +
					"whenever a claim is matched: \"this project path\" and \"this project path as " +
					"attested by our GitLab\" are different statements.",
			},
			"subject": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Token name, or the OIDC `sub` claim. A trailing `*` makes it a prefix.",
			},
			"project_path": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "GitLab CI `project_path` claim, e.g. `platform/*`.",
			},
			"ref": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "GitLab CI `ref` claim, e.g. `refs/heads/main` — which is how " +
					"\"only from the default branch\" is expressed.",
			},
			"authenticated": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Require any non-anonymous identity. It is what \"anyone who " +
					"signed in\" means, without naming who.",
			},
		},
	}
}

func (r *bindingResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = configureResource(req, resp)
}

func (r *bindingResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var model bindingModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}

	switch kind := model.Kind.ValueString(); {
	case model.Kind.IsNull() || model.Kind.IsUnknown() || kind == "":
	case kind == "token", kind == "oidc", kind == "anonymous":
	default:
		resp.Diagnostics.AddAttributeError(pathOf("kind"), "Unknown identity kind",
			kind+" is not one of token, oidc, anonymous.")
	}
	if model.Kind.ValueString() == "token" && model.Issuer.ValueString() != "" {
		resp.Diagnostics.AddAttributeError(pathOf("issuer"), "A static token has no issuer",
			"Issuer applies to OIDC identities.")
	}

	// A binding with nothing to match on selects everybody. That is a real
	// thing to want — it is how a public feed is expressed — but it is
	// almost never what somebody meant to write in Terraform, so say so.
	if model.Kind.ValueString() == "" && model.Subject.ValueString() == "" &&
		model.ProjectPath.ValueString() == "" && model.Issuer.ValueString() == "" &&
		model.Ref.ValueString() == "" && !model.Authenticated.ValueBool() {
		resp.Diagnostics.AddWarning("This binding matches every identity",
			"With no conditions it applies to everyone, including anonymous callers. "+
				"Set authenticated = true for \"anyone who signed in\", or name a kind, "+
				"subject or claim.")
	}
	validateHTTPURL(&resp.Diagnostics, pathOf("issuer"), model.Issuer)
}

func (r *bindingResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan bindingModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.write(ctx, &plan, "creating binding "+plan.Name.ValueString(), &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *bindingResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state bindingModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()

	var binding client.Binding
	err := r.client.Get(ctx, "/config/access/bindings/"+name, &binding)
	if removedOutsideTerraform(ctx, err, resp) {
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read binding "+name, err.Error())
		return
	}

	state.ID = types.StringValue(binding.Name)
	state.Name = types.StringValue(binding.Name)
	policies, diags := stringsOrPrior(ctx, binding.Policies, state.Policies)
	resp.Diagnostics.Append(diags...)
	state.Policies = policies
	state.Kind = stringOrPrior(binding.Match.Kind, state.Kind)
	state.Issuer = stringOrPrior(binding.Match.Issuer, state.Issuer)
	state.Subject = stringOrPrior(binding.Match.Subject, state.Subject)
	state.ProjectPath = stringOrPrior(binding.Match.ProjectPath, state.ProjectPath)
	state.Ref = stringOrPrior(binding.Match.Ref, state.Ref)
	state.Authenticated = boolOrPrior(binding.Match.Authenticated, state.Authenticated)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *bindingResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan bindingModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.write(ctx, &plan, "updating binding "+plan.Name.ValueString(), &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *bindingResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state bindingModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()
	err := r.client.Delete(ctx, "/config/access/bindings/"+name)
	if err != nil && !isNotFound(err) {
		reportWriteError(&resp.Diagnostics, "deleting binding "+name, err)
	}
}

func (r *bindingResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

func (r *bindingResource) write(ctx context.Context, plan *bindingModel,
	action string, diags *diag.Diagnostics) {
	binding := client.Binding{
		Name:     plan.Name.ValueString(),
		Policies: stringsFrom(ctx, plan.Policies, diags),
		Match: client.BindingMatch{
			Kind:          plan.Kind.ValueString(),
			Issuer:        plan.Issuer.ValueString(),
			Subject:       plan.Subject.ValueString(),
			ProjectPath:   plan.ProjectPath.ValueString(),
			Ref:           plan.Ref.ValueString(),
			Authenticated: plan.Authenticated.ValueBool(),
		},
	}
	if diags.HasError() {
		return
	}
	if _, err := r.client.Put(ctx, "/config/access/bindings/"+binding.Name, binding); err != nil {
		reportWriteError(diags, action, err)
		return
	}
	plan.ID = types.StringValue(binding.Name)
}
