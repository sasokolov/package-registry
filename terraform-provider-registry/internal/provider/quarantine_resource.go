package provider

import (
	"context"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/sasokolov/package-registry/terraform-provider-registry/internal/client"
)

// conflictReason is the one reason a block cannot be declared: it is derived
// from a recorded cross-site conflict, and the way to clear it is to resolve
// the conflict, not to delete the symptom.
const conflictReason = "cross_site_conflict"

// NewQuarantineResource returns the registry_quarantine resource.
func NewQuarantineResource() resource.Resource { return &quarantineResource{} }

type quarantineResource struct {
	client *client.Client
}

type quarantineModel struct {
	ID         types.String `tfsdk:"id"`
	Feed       types.String `tfsdk:"feed"`
	Coordinate types.String `tfsdk:"coordinate"`
	Reason     types.String `tfsdk:"reason"`
	Detail     types.String `tfsdk:"detail"`
	CreatedAt  types.String `tfsdk:"created_at"`
}

func (r *quarantineResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_quarantine"
}

func (r *quarantineResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A blocked coordinate: the registry refuses to serve it with 409 while " +
			"this exists, at this site and at every site the block replicates to.\n\n" +
			"A block removes access, never bytes. Destroying the resource releases the block and " +
			"the coordinate is served again — which is why an incident response can be expressed " +
			"as code and reverted the same way.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`<feed>/<coordinate>/<reason>`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"feed": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Feed the coordinate belongs to.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"coordinate": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Canonical coordinate, e.g. `maven:com.example:lib@1.0.0` or " +
					"`npm:left-pad@1.3.0`.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"reason": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Why it is blocked. Defaults to `manual`. `" + conflictReason +
					"` is refused: that block is derived from a recorded conflict and is cleared by " +
					"resolving the conflict.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"detail": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Free text kept in the audit log — a CVE identifier, a ticket, " +
					"whatever the next person will need.",
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "When the block was recorded.",
			},
		},
	}
}

func (r *quarantineResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = configureResource(req, resp)
}

func (r *quarantineResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var model quarantineModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if model.Reason.ValueString() == conflictReason {
		resp.Diagnostics.AddAttributeError(pathOf("reason"), "This block cannot be declared",
			"A "+conflictReason+" block is derived from a recorded conflict between sites. "+
				"Resolve the conflict instead; the block clears itself.")
	}
}

func (r *quarantineResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan quarantineModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.block(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *quarantineResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state quarantineModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var list client.QuarantineList
	if err := r.client.Get(ctx, "/quarantine", &list); err != nil {
		resp.Diagnostics.AddError("Failed to read quarantine", err.Error())
		return
	}
	feed, coord, reason := state.Feed.ValueString(), state.Coordinate.ValueString(), state.Reason.ValueString()
	for _, entry := range list.Quarantine {
		if entry.Feed != feed || entry.Coordinate != coord || entry.Reason != reason {
			continue
		}
		state.ID = types.StringValue(quarantineID(feed, coord, reason))
		state.Detail = stringOrPrior(entry.Detail, state.Detail)
		state.CreatedAt = types.StringValue(entry.CreatedAt)
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
		return
	}
	// Released outside Terraform: the coordinate is being served again.
	resp.State.RemoveResource(ctx)
}

// Update re-blocks with the new detail. Only the detail can change; feed,
// coordinate and reason are the identity.
func (r *quarantineResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan quarantineModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.block(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete releases the block; the bytes were never touched.
func (r *quarantineResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state quarantineModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	inactive := false
	body := client.QuarantineRequest{
		Feed:       state.Feed.ValueString(),
		Coordinate: state.Coordinate.ValueString(),
		Reason:     state.Reason.ValueString(),
		Active:     &inactive,
	}
	if err := r.client.Post(ctx, "/quarantine", body, nil); err != nil && !isNotFound(err) {
		reportWriteError(&resp.Diagnostics, "releasing "+body.Coordinate, err)
	}
}

// ImportState takes "<feed>/<coordinate>/<reason>"; the reason may be left
// off and defaults to manual.
func (r *quarantineResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(req.ID, "/")
	if len(parts) < 2 {
		resp.Diagnostics.AddError("Cannot parse the import ID",
			"Expected \"<feed>/<coordinate>\" or \"<feed>/<coordinate>/<reason>\", got "+req.ID)
		return
	}
	feed, coordinate := parts[0], parts[1]
	reason := "manual"
	if len(parts) > 2 {
		reason = parts[2]
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("feed"), feed)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("coordinate"), coordinate)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("reason"), reason)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"),
		quarantineID(feed, coordinate, reason))...)
}

func (r *quarantineResource) block(ctx context.Context, plan *quarantineModel, diags *diag.Diagnostics) {
	reason := plan.Reason.ValueString()
	if reason == "" || plan.Reason.IsUnknown() {
		reason = "manual"
	}
	body := client.QuarantineRequest{
		Feed:       plan.Feed.ValueString(),
		Coordinate: plan.Coordinate.ValueString(),
		Reason:     reason,
		Detail:     plan.Detail.ValueString(),
	}
	if err := r.client.Post(ctx, "/quarantine", body, nil); err != nil {
		reportWriteError(diags, "blocking "+body.Coordinate, err)
		return
	}
	plan.Reason = types.StringValue(reason)
	plan.ID = types.StringValue(quarantineID(body.Feed, body.Coordinate, reason))

	// The registry stamps the block; read it back so state carries the real
	// time rather than the provider's guess at it.
	var list client.QuarantineList
	if err := r.client.Get(ctx, "/quarantine", &list); err == nil {
		for _, entry := range list.Quarantine {
			if entry.Feed == body.Feed && entry.Coordinate == body.Coordinate && entry.Reason == reason {
				plan.CreatedAt = types.StringValue(entry.CreatedAt)
				return
			}
		}
	}
	plan.CreatedAt = types.StringValue("")
}

func quarantineID(feed, coordinate, reason string) string {
	return feed + "/" + coordinate + "/" + reason
}
