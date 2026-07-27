package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/sasokolov/package-registry/terraform-provider-registry/internal/client"
)

// The data sources answer questions about a live site rather than about the
// configuration Terraform wrote: what this site calls itself, what it is
// actually serving, and how far behind its peers it is. That is what makes
// them useful in a plan — you can key a decision on the site you are talking
// to instead of hardcoding which one it is.

// ---------------------------------------------------------------------------
// registry_site

// NewSiteDataSource returns the registry_site data source.
func NewSiteDataSource() datasource.DataSource { return &siteDataSource{} }

type siteDataSource struct{ client *client.Client }

type siteModel struct {
	Site                types.String `tfsdk:"site"`
	ConfigVersion       types.String `tfsdk:"config_version"`
	ConfigSource        types.String `tfsdk:"config_source"`
	Feeds               types.Int64  `tfsdk:"feeds"`
	Database            types.String `tfsdk:"database"`
	ReplicationEnabled  types.Bool   `tfsdk:"replication_enabled"`
	ReplicationPeers    types.Int64  `tfsdk:"replication_peers"`
	ReplicationTopology types.String `tfsdk:"replication_topology"`
}

func (d *siteDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_site"
}

func (d *siteDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The site this provider is talking to.",
		Attributes: map[string]schema.Attribute{
			"site":                 schema.StringAttribute{Computed: true, MarkdownDescription: "Site name."},
			"config_version":       schema.StringAttribute{Computed: true, MarkdownDescription: "sha256 of the configuration document currently in force."},
			"config_source":        schema.StringAttribute{Computed: true, MarkdownDescription: "Where that document lives — a file, or an object in the blob store."},
			"feeds":                schema.Int64Attribute{Computed: true, MarkdownDescription: "How many feeds are configured."},
			"database":             schema.StringAttribute{Computed: true, MarkdownDescription: "`up`, `unavailable` or `disabled`. Reads survive `unavailable`; publishing does not."},
			"replication_enabled":  schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether this site federates."},
			"replication_peers":    schema.Int64Attribute{Computed: true, MarkdownDescription: "How many peers it is configured with."},
			"replication_topology": schema.StringAttribute{Computed: true, MarkdownDescription: "Federation topology."},
		},
	}
}

func (d *siteDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = configureDataSource(req, resp)
}

func (d *siteDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	var status client.Site
	if err := d.client.Get(ctx, "/status", &status); err != nil {
		resp.Diagnostics.AddError("Failed to read site status", err.Error())
		return
	}
	model := siteModel{
		Site:                types.StringValue(status.Site),
		ConfigVersion:       types.StringValue(status.ConfigVersion),
		ConfigSource:        types.StringValue(status.ConfigSource),
		Feeds:               types.Int64Value(int64(status.Feeds)),
		Database:            types.StringValue(status.Database),
		ReplicationEnabled:  types.BoolValue(status.Replication.Enabled),
		ReplicationPeers:    types.Int64Value(int64(status.Replication.Peers)),
		ReplicationTopology: types.StringValue(status.Replication.Topology),
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}

// ---------------------------------------------------------------------------
// registry_feed

// NewFeedDataSource returns the registry_feed data source.
func NewFeedDataSource() datasource.DataSource { return &feedDataSource{} }

type feedDataSource struct{ client *client.Client }

type feedDataModel struct {
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
	Members         types.List    `tfsdk:"members"`
	Policies        types.List    `tfsdk:"policies"`
}

func (d *feedDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_feed"
}

func (d *feedDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "One feed as the registry currently has it configured — including feeds " +
			"Terraform does not manage.",
		Attributes: map[string]schema.Attribute{
			"name":             schema.StringAttribute{Required: true, MarkdownDescription: "Feed name."},
			"format":           schema.StringAttribute{Computed: true},
			"upstream":         schema.StringAttribute{Computed: true},
			"anonymous":        schema.BoolAttribute{Computed: true},
			"hosted":           schema.BoolAttribute{Computed: true},
			"publishers":       schema.ListAttribute{Computed: true, ElementType: types.StringType},
			"upstream_rps":     schema.Float64Attribute{Computed: true},
			"redirect":         schema.BoolAttribute{Computed: true},
			"redirect_ttl":     schema.StringAttribute{Computed: true},
			"publish_policy":   schema.StringAttribute{Computed: true},
			"replication_mode": schema.StringAttribute{Computed: true},
			"peer_fallback":    schema.BoolAttribute{Computed: true},
			"members": schema.ListAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Group members, in order; empty for a feed that is not a group.",
			},
			"policies": schema.ListAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Policy names in chain order.",
			},
		},
	}
}

func (d *feedDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = configureDataSource(req, resp)
}

func (d *feedDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var model feedDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := model.Name.ValueString()

	var feed client.Feed
	if err := d.client.Get(ctx, "/config/feeds/"+name, &feed); err != nil {
		resp.Diagnostics.AddError("Failed to read feed "+name, err.Error())
		return
	}
	resp.Diagnostics.Append(feedDataFrom(ctx, feed, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}

// ---------------------------------------------------------------------------
// registry_feeds

// NewFeedsDataSource returns the registry_feeds data source.
func NewFeedsDataSource() datasource.DataSource { return &feedsDataSource{} }

type feedsDataSource struct{ client *client.Client }

type feedsModel struct {
	Names types.List `tfsdk:"names"`
	Feeds types.List `tfsdk:"feeds"`
}

func (d *feedsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_feeds"
}

func (d *feedsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every feed the site has configured. Useful for asserting that Terraform " +
			"owns all of them, or for finding the ones it does not.",
		Attributes: map[string]schema.Attribute{
			"names": schema.ListAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Feed names, in configuration order.",
			},
			"feeds": schema.ListNestedAttribute{
				Computed: true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"name":             schema.StringAttribute{Computed: true},
						"format":           schema.StringAttribute{Computed: true},
						"upstream":         schema.StringAttribute{Computed: true},
						"anonymous":        schema.BoolAttribute{Computed: true},
						"hosted":           schema.BoolAttribute{Computed: true},
						"publish_policy":   schema.StringAttribute{Computed: true},
						"replication_mode": schema.StringAttribute{Computed: true},
					},
				},
			},
		},
	}
}

func (d *feedsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = configureDataSource(req, resp)
}

func (d *feedsDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	var list client.FeedList
	if err := d.client.Get(ctx, "/config/feeds", &list); err != nil {
		resp.Diagnostics.AddError("Failed to list feeds", err.Error())
		return
	}

	names := make([]string, 0, len(list.Feeds))
	rows := make([]feedRow, 0, len(list.Feeds))
	for _, feed := range list.Feeds {
		names = append(names, feed.Name)
		rows = append(rows, feedRow{
			Name:            types.StringValue(feed.Name),
			Format:          types.StringValue(feed.Format),
			Upstream:        types.StringValue(feed.Upstream),
			Anonymous:       types.BoolValue(feed.Anonymous),
			Hosted:          types.BoolValue(feed.Hosted),
			PublishPolicy:   types.StringValue(feed.PublishPolicy),
			ReplicationMode: types.StringValue(feed.ReplicationMode),
		})
	}

	nameList, diags := types.ListValueFrom(ctx, types.StringType, names)
	resp.Diagnostics.Append(diags...)
	feedList, diags := types.ListValueFrom(ctx, feedRowType(), rows)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &feedsModel{Names: nameList, Feeds: feedList})...)
}

type feedRow struct {
	Name            types.String `tfsdk:"name"`
	Format          types.String `tfsdk:"format"`
	Upstream        types.String `tfsdk:"upstream"`
	Anonymous       types.Bool   `tfsdk:"anonymous"`
	Hosted          types.Bool   `tfsdk:"hosted"`
	PublishPolicy   types.String `tfsdk:"publish_policy"`
	ReplicationMode types.String `tfsdk:"replication_mode"`
}

// ---------------------------------------------------------------------------
// registry_replication_status

// NewReplicationStatusDataSource returns the registry_replication_status data
// source.
func NewReplicationStatusDataSource() datasource.DataSource { return &replicationStatusDataSource{} }

type replicationStatusDataSource struct{ client *client.Client }

type replicationStatusModel struct {
	Enabled types.Bool   `tfsdk:"enabled"`
	Site    types.String `tfsdk:"site"`
	Parked  types.Int64  `tfsdk:"parked"`
	Cursors types.List   `tfsdk:"cursors"`
}

func (d *replicationStatusDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_replication_status"
}

func (d *replicationStatusDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "How this site's federation is doing right now: how far each stream has " +
			"been applied, how much of that is durable (the real RPO), and how many events are parked.",
		Attributes: map[string]schema.Attribute{
			"enabled": schema.BoolAttribute{Computed: true},
			"site":    schema.StringAttribute{Computed: true},
			"parked": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Events this site received but cannot apply yet. Persistently non-zero is a problem.",
			},
			"cursors": schema.ListNestedAttribute{
				Computed: true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"peer":        schema.StringAttribute{Computed: true},
						"origin":      schema.StringAttribute{Computed: true},
						"applied_seq": schema.Int64Attribute{Computed: true},
						"durable_seq": schema.Int64Attribute{Computed: true, MarkdownDescription: "The durability watermark: everything at or below it survives losing this site."},
						"last_error":  schema.StringAttribute{Computed: true},
					},
				},
			},
		},
	}
}

func (d *replicationStatusDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = configureDataSource(req, resp)
}

func (d *replicationStatusDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	var status client.ReplicationStatus
	if err := d.client.Get(ctx, "/replication", &status); err != nil {
		resp.Diagnostics.AddError("Failed to read replication status", err.Error())
		return
	}

	rows := make([]cursorRow, 0, len(status.Cursors))
	for _, cursor := range status.Cursors {
		rows = append(rows, cursorRow{
			Peer:       types.StringValue(cursor.Peer),
			Origin:     types.StringValue(cursor.Origin),
			AppliedSeq: types.Int64Value(cursor.AppliedSeq),
			DurableSeq: types.Int64Value(cursor.DurableSeq),
			LastError:  types.StringValue(cursor.LastError),
		})
	}
	cursors, diags := types.ListValueFrom(ctx, cursorRowType(), rows)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &replicationStatusModel{
		Enabled: types.BoolValue(status.Enabled),
		Site:    types.StringValue(status.Site),
		Parked:  types.Int64Value(int64(status.Parked)),
		Cursors: cursors,
	})...)
}

type cursorRow struct {
	Peer       types.String `tfsdk:"peer"`
	Origin     types.String `tfsdk:"origin"`
	AppliedSeq types.Int64  `tfsdk:"applied_seq"`
	DurableSeq types.Int64  `tfsdk:"durable_seq"`
	LastError  types.String `tfsdk:"last_error"`
}
