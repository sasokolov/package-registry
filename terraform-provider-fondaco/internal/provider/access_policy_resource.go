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

	"github.com/fondaco-dev/fondaco/terraform-provider-fondaco/internal/client"
)

// capabilities the registry understands. They are listed here so a typo
// fails at plan time next to the line that caused it; the registry checks
// them too, because it is the one that has to be right.
var knownCapabilities = map[string]bool{
	"read": true, "list": true, "publish": true,
	"create": true, "update": true, "delete": true, "deny": true,
}

// NewAccessPolicyResource returns the fondaco_access_policy resource.
func NewAccessPolicyResource() resource.Resource { return &accessPolicyResource{} }

type accessPolicyResource struct {
	client *client.Client
}

type accessPolicyModel struct {
	ID    types.String      `tfsdk:"id"`
	Name  types.String      `tfsdk:"name"`
	Rules []accessRuleModel `tfsdk:"rule"`
}

type accessRuleModel struct {
	Path         types.String `tfsdk:"path"`
	Capabilities types.List   `tfsdk:"capabilities"`
}

func (r *accessPolicyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_access_policy"
}

func (r *accessPolicyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A named set of access rules.\n\n" +
			"A policy says what may be done on which paths; a " +
			"`fondaco_binding` says whose identity it applies to. Nothing is " +
			"permitted until a policy says so.\n\n" +
			"Paths are objects of the registry, not URLs:\n\n" +
			"* `feed/<feed>/<coordinate>` — what the registry serves, e.g. " +
			"`feed/releases/maven:com.example:lib@1.0.0`\n" +
			"* `sys/<area>` — how it is run: `sys/config`, `sys/tokens`, " +
			"`sys/quarantine`, `sys/conflicts`, `sys/replication`, `sys/status`, `sys/feeds`\n\n" +
			"A trailing `*` matches the remainder and `+` matches one segment. " +
			"The most specific matching rule decides, and a `deny` beats every " +
			"other capability at that specificity — which is what lets a narrow " +
			"rule be a deliberate exception to a broad one, in either direction.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The policy name, which is its identity.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Policy name. Bindings refer to it, and an explanation of a " +
					"refusal names it, so it is worth naming for the person who will read that.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
		},
		Blocks: map[string]schema.Block{
			"rule": schema.ListNestedBlock{
				MarkdownDescription: "One rule. Order does not matter: specificity decides.",
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"path": schema.StringAttribute{
							Required:            true,
							MarkdownDescription: "The path this rule is about.",
						},
						"capabilities": schema.ListAttribute{
							Required:    true,
							ElementType: types.StringType,
							MarkdownDescription: "One or more of `read`, `list`, `publish`, `create`, " +
								"`update`, `delete`, or `deny` on its own to refuse.",
						},
					},
				},
			},
		},
	}
}

func (r *accessPolicyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = configureResource(req, resp)
}

// ValidateConfig catches the mistakes that can be caught without seeing the
// rest of the document.
func (r *accessPolicyResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var model accessPolicyModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if name := model.Name.ValueString(); name != "" && !model.Name.IsUnknown() {
		if strings.HasPrefix(name, "feed:") || strings.HasPrefix(name, "sys:") {
			resp.Diagnostics.AddAttributeError(pathOf("name"), "Reserved policy name",
				"Names starting with feed: or sys: belong to the policies the registry generates "+
					"from a feed's anonymous/publishers and the site's admins.")
		}
	}
	if len(model.Rules) == 0 {
		resp.Diagnostics.AddError("A policy with no rules grants nothing",
			"Add at least one rule block, or remove the policy.")
	}

	for i, rule := range model.Rules {
		at := path.Root("rule").AtListIndex(i)
		rulePath := rule.Path.ValueString()
		switch {
		case rule.Path.IsUnknown() || rulePath == "":
		case !strings.HasPrefix(rulePath, "feed/") && !strings.HasPrefix(rulePath, "sys/"):
			resp.Diagnostics.AddAttributeError(at.AtName("path"), "Path is in no namespace",
				"A path must start with feed/ or sys/; got "+rulePath)
		case strings.Contains(strings.TrimSuffix(rulePath, "*"), "*"):
			resp.Diagnostics.AddAttributeError(at.AtName("path"), "Misplaced wildcard",
				`"*" is only allowed at the end; use "+" to match exactly one segment.`)
		}

		if rule.Capabilities.IsNull() || rule.Capabilities.IsUnknown() {
			continue
		}
		var caps []string
		resp.Diagnostics.Append(rule.Capabilities.ElementsAs(ctx, &caps, false)...)
		if len(caps) == 0 {
			resp.Diagnostics.AddAttributeError(at.AtName("capabilities"), "A rule that grants nothing",
				"List at least one capability, or use deny to refuse.")
		}
		for _, capability := range caps {
			if !knownCapabilities[capability] {
				resp.Diagnostics.AddAttributeError(at.AtName("capabilities"),
					"Unknown capability",
					capability+" is not one of read, list, publish, create, update, delete, deny.")
			}
		}
		if len(caps) > 1 {
			for _, capability := range caps {
				if capability == "deny" {
					resp.Diagnostics.AddAttributeWarning(at.AtName("capabilities"),
						"deny makes the other capabilities on this rule meaningless",
						"A deny refuses the path outright. Listing it beside grants reads as though "+
							"the grants still apply; they do not.")
				}
			}
		}
	}
}

func (r *accessPolicyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan accessPolicyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.write(ctx, &plan, "creating access policy "+plan.Name.ValueString(), &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *accessPolicyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state accessPolicyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()

	var policy client.AccessPolicy
	err := r.client.Get(ctx, "/config/access/policies/"+name, &policy)
	if removedOutsideTerraform(ctx, err, resp) {
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read access policy "+name, err.Error())
		return
	}

	state.ID = types.StringValue(policy.Name)
	state.Name = types.StringValue(policy.Name)
	rules := make([]accessRuleModel, 0, len(policy.Rules))
	for i, rule := range policy.Rules {
		var prior types.List
		if i < len(state.Rules) {
			prior = state.Rules[i].Capabilities
		} else {
			prior = types.ListNull(types.StringType)
		}
		caps, diags := stringsOrPrior(ctx, rule.Capabilities, prior)
		resp.Diagnostics.Append(diags...)
		rules = append(rules, accessRuleModel{Path: types.StringValue(rule.Path), Capabilities: caps})
	}
	state.Rules = rules
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *accessPolicyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan accessPolicyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.write(ctx, &plan, "updating access policy "+plan.Name.ValueString(), &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *accessPolicyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state accessPolicyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()
	err := r.client.Delete(ctx, "/config/access/policies/"+name)
	if err != nil && !isNotFound(err) {
		reportWriteError(&resp.Diagnostics, "deleting access policy "+name, err)
	}
}

func (r *accessPolicyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

func (r *accessPolicyResource) write(ctx context.Context, plan *accessPolicyModel,
	action string, diags *diag.Diagnostics) {
	policy := client.AccessPolicy{Name: plan.Name.ValueString()}
	for _, rule := range plan.Rules {
		policy.Rules = append(policy.Rules, client.AccessRule{
			Path:         rule.Path.ValueString(),
			Capabilities: stringsFrom(ctx, rule.Capabilities, diags),
		})
	}
	if diags.HasError() {
		return
	}
	if _, err := r.client.Put(ctx, "/config/access/policies/"+policy.Name, policy); err != nil {
		reportWriteError(diags, action, err)
		return
	}
	plan.ID = types.StringValue(policy.Name)
}
