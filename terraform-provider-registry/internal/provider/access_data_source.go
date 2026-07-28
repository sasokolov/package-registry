package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/sasokolov/package-registry/terraform-provider-registry/internal/client"
)

// NewAccessDataSource returns the registry_access_explain data source.
func NewAccessDataSource() datasource.DataSource { return &accessDataSource{} }

type accessDataSource struct{ client *client.Client }

type accessExplainModel struct {
	Path         types.String `tfsdk:"path"`
	Capability   types.String `tfsdk:"capability"`
	Kind         types.String `tfsdk:"kind"`
	Subject      types.String `tfsdk:"subject"`
	Issuer       types.String `tfsdk:"issuer"`
	ProjectPath  types.String `tfsdk:"project_path"`
	Ref          types.String `tfsdk:"ref"`
	Allowed      types.Bool   `tfsdk:"allowed"`
	Reason       types.String `tfsdk:"reason"`
	Policy       types.String `tfsdk:"policy"`
	Rule         types.String `tfsdk:"rule"`
	Policies     types.List   `tfsdk:"policies"`
	Bindings     types.List   `tfsdk:"bindings"`
	Capabilities types.List   `tfsdk:"effective_capabilities"`
}

func (d *accessDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_access_explain"
}

func (d *accessDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Asks the registry what its rules would decide, and why.\n\n" +
			"It is meant for assertions: a `check` block or a `precondition` that says " +
			"\"this pipeline must be able to publish here, and that one must not\" turns " +
			"an intention into something a plan can fail on. Reading it needs permission " +
			"to read the access rules, because it answers about identities other than " +
			"your own.",
		Attributes: map[string]schema.Attribute{
			"path": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The path to ask about, e.g. `feed/releases/maven:com.example:lib@1.0.0`.",
			},
			"capability": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Which capability to ask about. Defaults to `read`.",
			},
			"kind":         schema.StringAttribute{Optional: true, MarkdownDescription: "Identity kind to ask about; defaults to the provider's own identity."},
			"subject":      schema.StringAttribute{Optional: true, MarkdownDescription: "Token name or OIDC subject."},
			"issuer":       schema.StringAttribute{Optional: true, MarkdownDescription: "OIDC issuer."},
			"project_path": schema.StringAttribute{Optional: true, MarkdownDescription: "GitLab CI project_path claim."},
			"ref":          schema.StringAttribute{Optional: true, MarkdownDescription: "GitLab CI ref claim."},

			"allowed": schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether it would be allowed."},
			"reason":  schema.StringAttribute{Computed: true, MarkdownDescription: "One sentence saying why."},
			"policy":  schema.StringAttribute{Computed: true, MarkdownDescription: "The policy that decided."},
			"rule":    schema.StringAttribute{Computed: true, MarkdownDescription: "The rule path that decided."},
			"policies": schema.ListAttribute{
				Computed: true, ElementType: types.StringType,
				MarkdownDescription: "Every policy bound to that identity.",
			},
			"bindings": schema.ListAttribute{
				Computed: true, ElementType: types.StringType,
				MarkdownDescription: "The bindings that brought those policies into play. " +
					"Empty means no binding matched the identity at all, which is a " +
					"different mistake from a policy that grants too little.",
			},
			"effective_capabilities": schema.ListAttribute{
				Computed: true, ElementType: types.StringType,
				MarkdownDescription: "What is granted at the deciding path.",
			},
		},
	}
}

func (d *accessDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = configureDataSource(req, resp)
}

func (d *accessDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var model accessExplainModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}

	query := "/access/explain?path=" + client.Escape(model.Path.ValueString())
	for key, value := range map[string]string{
		"capability":   model.Capability.ValueString(),
		"kind":         model.Kind.ValueString(),
		"subject":      model.Subject.ValueString(),
		"issuer":       model.Issuer.ValueString(),
		"project_path": model.ProjectPath.ValueString(),
		"ref":          model.Ref.ValueString(),
	} {
		if value != "" {
			query += "&" + key + "=" + client.Escape(value)
		}
	}

	var explanation client.Explanation
	if err := d.client.Get(ctx, query, &explanation); err != nil {
		resp.Diagnostics.AddError("Failed to ask the registry what it would decide", err.Error())
		return
	}

	model.Allowed = types.BoolValue(explanation.Allowed)
	model.Reason = types.StringValue(explanation.Reason)
	model.Policy = types.StringValue(explanation.Policy)
	model.Rule = types.StringValue(explanation.Rule)
	model.Capability = types.StringValue(explanation.Capability)

	policies, diags := types.ListValueFrom(ctx, types.StringType, explanation.Policies)
	resp.Diagnostics.Append(diags...)
	model.Policies = policies
	bindings, diags := types.ListValueFrom(ctx, types.StringType, explanation.Bindings)
	resp.Diagnostics.Append(diags...)
	model.Bindings = bindings
	caps, diags := types.ListValueFrom(ctx, types.StringType, explanation.Capabilities)
	resp.Diagnostics.Append(diags...)
	model.Capabilities = caps
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}
