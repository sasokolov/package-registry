package provider

import (
	"context"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/sasokolov/package-registry/terraform-provider-registry/internal/client"
)

// NewReplicationPeerResource returns the registry_replication_peer resource.
func NewReplicationPeerResource() resource.Resource { return &replicationPeerResource{} }

type replicationPeerResource struct {
	client *client.Client
}

type replicationPeerModel struct {
	ID           types.String `tfsdk:"id"`
	Name         types.String `tfsdk:"name"`
	URL          types.String `tfsdk:"url"`
	PublicURL    types.String `tfsdk:"public_url"`
	PullInterval types.String `tfsdk:"pull_interval"`
	TokenFile    types.String `tfsdk:"token_file"`
}

func (r *replicationPeerResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_replication_peer"
}

func (r *replicationPeerResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "One geo-replication partner of this site: where to pull its journal " +
			"from, and where to forward publishes for feeds homed there.\n\n" +
			"Credentials are referenced by file path, never inlined: a peer token belongs in a " +
			"mounted secret, not in Terraform state.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The peer name, which is its identity.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Peer site name. It must match the name that site calls itself, " +
					"because it is what feeds refer to in `publish_policy = \"forward:<site>\"`.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"url": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The peer's internal replication API — the listener that carries " +
					"the journal, never the public one.",
			},
			"public_url": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "The peer's client-facing base URL. Required if any feed here is " +
					"homed at this peer, because that is where its publishes are forwarded.",
			},
			"pull_interval": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "How often to poll this peer's journal, e.g. `2s`.",
			},
			"token_file": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Path to the file holding the credential presented to this peer " +
					"(bearer mode). The registry reads it; Terraform never sees its contents.",
			},
		},
	}
}

func (r *replicationPeerResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = configureResource(req, resp)
}

func (r *replicationPeerResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var model replicationPeerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}
	validateHTTPURL(&resp.Diagnostics, pathOf("url"), model.URL)
	validateHTTPURL(&resp.Diagnostics, pathOf("public_url"), model.PublicURL)
	if raw := model.PullInterval.ValueString(); raw != "" && !model.PullInterval.IsUnknown() {
		if _, err := time.ParseDuration(raw); err != nil {
			resp.Diagnostics.AddAttributeError(pathOf("pull_interval"), "Invalid duration",
				"Expected something like \"2s\": "+err.Error())
		}
	}
}

func (r *replicationPeerResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan replicationPeerModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.write(ctx, &plan, "adding replication peer "+plan.Name.ValueString(), &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *replicationPeerResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state replicationPeerModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()

	var peer client.Peer
	err := r.client.Get(ctx, "/config/peers/"+name, &peer)
	if removedOutsideTerraform(ctx, err, resp) {
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read replication peer "+name, err.Error())
		return
	}

	state.ID = types.StringValue(peer.Name)
	state.Name = types.StringValue(peer.Name)
	state.URL = types.StringValue(peer.URL)
	state.PublicURL = stringOrPrior(peer.PublicURL, state.PublicURL)
	state.PullInterval = durationOrPrior(peer.PullInterval, state.PullInterval)
	state.TokenFile = stringOrPrior(peer.TokenFile, state.TokenFile)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *replicationPeerResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan replicationPeerModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.write(ctx, &plan, "updating replication peer "+plan.Name.ValueString(), &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *replicationPeerResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state replicationPeerModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()
	err := r.client.Delete(ctx, "/config/peers/"+name)
	if err != nil && !isNotFound(err) {
		reportWriteError(&resp.Diagnostics, "removing replication peer "+name, err)
	}
}

func (r *replicationPeerResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

func (r *replicationPeerResource) write(ctx context.Context, plan *replicationPeerModel, action string, diags *diag.Diagnostics) {
	peer := client.Peer{
		Name:         plan.Name.ValueString(),
		URL:          plan.URL.ValueString(),
		PublicURL:    plan.PublicURL.ValueString(),
		PullInterval: plan.PullInterval.ValueString(),
		TokenFile:    plan.TokenFile.ValueString(),
	}
	if _, err := r.client.Put(ctx, "/config/peers/"+peer.Name, peer); err != nil {
		reportWriteError(diags, action, err)
		return
	}
	plan.ID = types.StringValue(peer.Name)
}
