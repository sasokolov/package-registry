package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
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

// Names the registry itself enforces. They are checked here too so a typo
// fails at plan time, next to the line that caused it, instead of after an
// apply has already started changing other things.
var (
	feedNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	formatRE   = regexp.MustCompile(`^[a-z0-9]+$`)
	// reservedFeedNames would shadow the registry's own API and console.
	reservedFeedNames = map[string]string{"api": "the registry's API", "ui": "the web console"}
)

// NewFeedResource returns the registry_feed resource.
func NewFeedResource() resource.Resource { return &feedResource{} }

type feedResource struct {
	client *client.Client
}

type feedModel struct {
	ID              types.String  `tfsdk:"id"`
	Name            types.String  `tfsdk:"name"`
	Format          types.String  `tfsdk:"format"`
	Upstream        types.String  `tfsdk:"upstream"`
	Anonymous       types.Bool    `tfsdk:"anonymous"`
	Hosted          types.Bool    `tfsdk:"hosted"`
	Publishers      types.List    `tfsdk:"publishers"`
	UpstreamRPS     types.Float64 `tfsdk:"upstream_rps"`
	Redirect        types.Bool    `tfsdk:"redirect"`
	RedirectTTL     types.String  `tfsdk:"redirect_ttl"`
	PublishPolicy   types.String  `tfsdk:"publish_policy"`
	ReplicationMode types.String  `tfsdk:"replication_mode"`
	PeerFallback    types.Bool    `tfsdk:"peer_fallback"`
	Policies        []policyModel `tfsdk:"policy"`
}

type policyModel struct {
	Name   types.String `tfsdk:"name"`
	Config types.String `tfsdk:"config"`
}

func (r *feedResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_feed"
}

func (r *feedResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "One feed: what it proxies, what it hosts, who may read and publish, " +
			"and how it behaves in a federation.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The feed name, which is its identity in the configuration.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Feed name, lowercase alphanumeric with dashes. It is the first " +
					"path segment clients use, so renaming it is a new feed, not an edit.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"format": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Package format module, e.g. `maven`, `npm`, `nuget`, `composer`, " +
					"`terraform`. Changing it changes how every stored path is interpreted, so it " +
					"replaces the feed.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"upstream": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Upstream base URL to proxy and cache. Omit for a purely hosted feed.",
			},
			"anonymous": schema.BoolAttribute{
				Optional:            true,
				MarkdownDescription: "Allow unauthenticated reads. Defaults to `false`.",
			},
			"hosted": schema.BoolAttribute{
				Optional:            true,
				MarkdownDescription: "Accept locally published packages. Requires a database.",
			},
			"publishers": schema.ListAttribute{
				Optional:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Identity patterns allowed to publish, e.g. `token:ci-*`, " +
					"`project:group/*`. Requires `hosted = true`; an empty list means publishing is off.",
			},
			"upstream_rps": schema.Float64Attribute{
				Optional:            true,
				MarkdownDescription: "Rate limit toward the upstream, in requests per second. `0` is unlimited.",
			},
			"redirect": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Serve cached artifacts as a redirect to a pre-signed storage URL " +
					"instead of streaming them. Honoured only where both the storage and the format allow it.",
			},
			"redirect_ttl": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Lifetime of a pre-signed URL, e.g. `15m`.",
			},
			"publish_policy": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Write model in a federation: `forward:<site>` (write-affinity, " +
					"the default model) or `local` (active-active, conflicts resolved by rule K1).",
			},
			"replication_mode": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "`eager` (blobs replicate ahead of demand, so the durability " +
					"watermark is a real RPO) or `lazy` (fetched from peers on demand). Defaults to `lazy`.",
			},
			"peer_fallback": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Let the read path fetch missing hosted content from peers, hiding " +
					"replication lag from clients.",
			},
		},
		Blocks: map[string]schema.Block{
			"policy": schema.ListNestedBlock{
				MarkdownDescription: "The feed's policy chain, in order. Each block is one policy.",
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"name": schema.StringAttribute{
							Required:            true,
							MarkdownDescription: "Registered policy name, e.g. `allowlist`, `osv`, `license`.",
						},
						"config": schema.StringAttribute{
							Optional: true,
							MarkdownDescription: "Policy options as JSON, usually written with " +
								"`jsonencode({...})`. The registry validates them; the provider only " +
								"checks that this is JSON, so a policy it has never heard of still works.",
						},
					},
				},
			},
		},
	}
}

func (r *feedResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = configureResource(req, resp)
}

// ValidateConfig applies the rules the registry applies, at plan time. It is
// deliberately a subset: anything that depends on the rest of the document
// (a forward: target that must be a known peer, a format module that must be
// compiled in) belongs to the registry, which sees the whole document.
func (r *feedResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var model feedModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if name := model.Name.ValueString(); name != "" && !model.Name.IsUnknown() {
		if !feedNameRE.MatchString(name) {
			resp.Diagnostics.AddAttributeError(pathOf("name"), "Invalid feed name",
				fmt.Sprintf("%q must match %s: it becomes a URL path segment.", name, feedNameRE))
		}
		if what, reserved := reservedFeedNames[name]; reserved {
			resp.Diagnostics.AddAttributeError(pathOf("name"), "Reserved feed name",
				fmt.Sprintf("%q is reserved for %s; a feed with this name would shadow it.", name, what))
		}
	}
	if format := model.Format.ValueString(); format != "" && !model.Format.IsUnknown() &&
		!formatRE.MatchString(format) {
		resp.Diagnostics.AddAttributeError(pathOf("format"), "Invalid format",
			fmt.Sprintf("%q must match %s.", format, formatRE))
	}

	// A feed that neither proxies nor hosts serves nothing at all.
	if !model.Upstream.IsUnknown() && !model.Hosted.IsUnknown() &&
		model.Upstream.ValueString() == "" && !model.Hosted.ValueBool() {
		resp.Diagnostics.AddError("Feed serves nothing",
			"A feed needs an upstream, hosted = true, or both.")
	}
	if !model.Publishers.IsNull() && !model.Publishers.IsUnknown() &&
		len(model.Publishers.Elements()) > 0 && !model.Hosted.IsUnknown() && !model.Hosted.ValueBool() {
		resp.Diagnostics.AddAttributeError(pathOf("publishers"), "Publishers require a hosted feed",
			"Set hosted = true, or remove publishers: a proxy feed has nothing to publish to.")
	}

	if ttl := model.RedirectTTL.ValueString(); ttl != "" && !model.RedirectTTL.IsUnknown() {
		if _, err := time.ParseDuration(ttl); err != nil {
			resp.Diagnostics.AddAttributeError(pathOf("redirect_ttl"), "Invalid duration",
				fmt.Sprintf("%q is not a duration such as \"15m\": %s", ttl, err))
		}
	}
	switch mode := model.ReplicationMode.ValueString(); {
	case model.ReplicationMode.IsUnknown(), mode == "", mode == "lazy", mode == "eager":
	default:
		resp.Diagnostics.AddAttributeError(pathOf("replication_mode"), "Unsupported replication mode",
			fmt.Sprintf("%q is not supported; use \"lazy\" or \"eager\".", mode))
	}
	switch policy := model.PublishPolicy.ValueString(); {
	case model.PublishPolicy.IsUnknown(), policy == "", policy == "local":
	case strings.HasPrefix(policy, "forward:"):
		if strings.TrimPrefix(policy, "forward:") == "" {
			resp.Diagnostics.AddAttributeError(pathOf("publish_policy"), "Missing forward target",
				"publish_policy = \"forward:\" needs the name of the home site.")
		}
	default:
		resp.Diagnostics.AddAttributeError(pathOf("publish_policy"), "Unsupported publish policy",
			fmt.Sprintf("%q is not supported; use \"local\" or \"forward:<site>\".", policy))
	}

	for i, policy := range model.Policies {
		if raw := policy.Config.ValueString(); raw != "" && !policy.Config.IsUnknown() {
			var into map[string]any
			if err := json.Unmarshal([]byte(raw), &into); err != nil {
				resp.Diagnostics.AddAttributeError(
					path.Root("policy").AtListIndex(i).AtName("config"),
					"Policy config is not a JSON object",
					"Write it with jsonencode({...}): "+err.Error())
			}
		}
	}
}

func (r *feedResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan feedModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.write(ctx, &plan, "creating feed "+plan.Name.ValueString(), &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *feedResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state feedModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var feed client.Feed
	err := r.client.Get(ctx, "/config/feeds/"+state.Name.ValueString(), &feed)
	if removedOutsideTerraform(ctx, err, resp) {
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read feed "+state.Name.ValueString(), err.Error())
		return
	}

	resp.Diagnostics.Append(feedToModel(ctx, feed, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *feedResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan feedModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.write(ctx, &plan, "updating feed "+plan.Name.ValueString(), &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete removes the feed from the configuration. The packages it served stay
// in storage: a configuration change never destroys published bytes.
func (r *feedResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state feedModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	err := r.client.Delete(ctx, "/config/feeds/"+state.Name.ValueString())
	if err != nil && !isNotFound(err) {
		reportWriteError(&resp.Diagnostics, "deleting feed "+state.Name.ValueString(), err)
	}
}

func (r *feedResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

// write sends the whole feed. The registry replaces it as a unit, so there is
// no partial update to get wrong.
func (r *feedResource) write(ctx context.Context, plan *feedModel, action string, diags *diag.Diagnostics) {
	feed := client.Feed{
		Name:            plan.Name.ValueString(),
		Format:          plan.Format.ValueString(),
		Upstream:        plan.Upstream.ValueString(),
		Anonymous:       plan.Anonymous.ValueBool(),
		Hosted:          plan.Hosted.ValueBool(),
		Publishers:      stringsFrom(ctx, plan.Publishers, diags),
		UpstreamRPS:     plan.UpstreamRPS.ValueFloat64(),
		Redirect:        plan.Redirect.ValueBool(),
		RedirectTTL:     plan.RedirectTTL.ValueString(),
		PublishPolicy:   plan.PublishPolicy.ValueString(),
		ReplicationMode: plan.ReplicationMode.ValueString(),
		PeerFallback:    plan.PeerFallback.ValueBool(),
	}
	for i, policy := range plan.Policies {
		entry := client.Policy{Name: policy.Name.ValueString()}
		if raw := policy.Config.ValueString(); raw != "" {
			if err := json.Unmarshal([]byte(raw), &entry.Options); err != nil {
				diags.AddAttributeError(
					path.Root("policy").AtListIndex(i).AtName("config"),
					"Policy config is not a JSON object", err.Error())
				return
			}
		}
		feed.Policies = append(feed.Policies, entry)
	}
	if diags.HasError() {
		return
	}

	if _, err := r.client.Put(ctx, "/config/feeds/"+feed.Name, feed); err != nil {
		reportWriteError(diags, action, err)
		return
	}
	plan.ID = types.StringValue(feed.Name)
}

// feedToModel copies what the registry reports into state.
//
// Optional attributes are written back only when the registry actually has a
// value, so a field the operator never set does not start appearing in every
// plan as a change from null to "".
func feedToModel(ctx context.Context, feed client.Feed, model *feedModel) diag.Diagnostics {
	var diags diag.Diagnostics

	model.ID = types.StringValue(feed.Name)
	model.Name = types.StringValue(feed.Name)
	model.Format = types.StringValue(feed.Format)
	model.Upstream = stringOrPrior(feed.Upstream, model.Upstream)
	model.Anonymous = boolOrPrior(feed.Anonymous, model.Anonymous)
	model.Hosted = boolOrPrior(feed.Hosted, model.Hosted)
	model.UpstreamRPS = float64OrPrior(feed.UpstreamRPS, model.UpstreamRPS)
	model.Redirect = boolOrPrior(feed.Redirect, model.Redirect)
	model.RedirectTTL = durationOrPrior(feed.RedirectTTL, model.RedirectTTL)
	model.PublishPolicy = stringOrPrior(feed.PublishPolicy, model.PublishPolicy)
	model.ReplicationMode = stringOrPrior(feed.ReplicationMode, model.ReplicationMode)
	model.PeerFallback = boolOrPrior(feed.PeerFallback, model.PeerFallback)

	publishers, d := stringsOrPrior(ctx, feed.Publishers, model.Publishers)
	diags.Append(d...)
	model.Publishers = publishers

	if len(feed.Policies) == 0 {
		model.Policies = nil
		return diags
	}
	policies := make([]policyModel, 0, len(feed.Policies))
	for i, policy := range feed.Policies {
		entry := policyModel{Name: types.StringValue(policy.Name)}
		var prior types.String
		if i < len(model.Policies) && model.Policies[i].Name.ValueString() == policy.Name {
			prior = model.Policies[i].Config
		} else {
			prior = types.StringNull()
		}
		config, err := jsonOrPrior(policy.Options, prior)
		if err != nil {
			diags.AddError("Cannot render policy config for "+policy.Name, err.Error())
			continue
		}
		entry.Config = config
		policies = append(policies, entry)
	}
	model.Policies = policies
	return diags
}
