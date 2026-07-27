package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/sasokolov/package-registry/terraform-provider-registry/internal/client"
)

// NewAdminBindingResource returns the registry_admin_binding resource.
func NewAdminBindingResource() resource.Resource { return &adminBindingResource{} }

type adminBindingResource struct {
	client *client.Client
}

type adminBindingModel struct {
	ID      types.String `tfsdk:"id"`
	Pattern types.String `tfsdk:"pattern"`
}

func (r *adminBindingResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_admin_binding"
}

func (r *adminBindingResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "One identity pattern allowed to administer the registry: to read and " +
			"change the configuration document, issue and revoke tokens, and quarantine coordinates.\n\n" +
			"Each binding owns exactly one pattern and leaves the rest of the list alone, so several " +
			"teams can manage their own access without rewriting each other's.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The pattern, which is its identity.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"pattern": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Identity pattern, in the same language as a feed's publishers: " +
					"`token:ops-*`, `project:infra/*`, `sub:...`. Editing it is a different binding, " +
					"so the old one is removed and the new one added.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
		},
	}
}

func (r *adminBindingResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = configureResource(req, resp)
}

func (r *adminBindingResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan adminBindingModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	pattern := plan.Pattern.ValueString()
	if _, err := r.client.Put(ctx, client.Query("/config/admins/binding", "pattern", pattern), nil); err != nil {
		reportWriteError(&resp.Diagnostics, "adding administrator "+pattern, err)
		return
	}
	plan.ID = types.StringValue(pattern)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *adminBindingResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state adminBindingModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	pattern := state.Pattern.ValueString()
	err := r.client.Get(ctx, client.Query("/config/admins/binding", "pattern", pattern), nil)
	if removedOutsideTerraform(ctx, err, resp) {
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read administrator "+pattern, err.Error())
		return
	}
	state.ID = types.StringValue(pattern)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update cannot happen: the only attribute is the identity, and changing it
// replaces the resource. The method exists because the interface requires it.
func (r *adminBindingResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan adminBindingModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *adminBindingResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state adminBindingModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	pattern := state.Pattern.ValueString()
	err := r.client.Delete(ctx, client.Query("/config/admins/binding", "pattern", pattern))
	if err != nil && !isNotFound(err) {
		// Removing the last administrator is refused by the registry, and
		// that refusal is worth passing through verbatim: it is the one
		// change that would leave nobody able to undo it.
		reportWriteError(&resp.Diagnostics, "removing administrator "+pattern, err)
	}
}

func (r *adminBindingResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("pattern"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
