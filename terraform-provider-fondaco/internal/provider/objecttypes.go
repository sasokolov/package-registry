package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/fondaco-dev/fondaco/terraform-provider-fondaco/internal/client"
)

// Object types for the nested lists the data sources return. They are spelled
// out once here so a schema and the value it produces cannot drift apart in
// two files.

func feedRowType() attr.Type {
	return types.ObjectType{AttrTypes: map[string]attr.Type{
		"name":             types.StringType,
		"format":           types.StringType,
		"upstream":         types.StringType,
		"anonymous":        types.BoolType,
		"hosted":           types.BoolType,
		"publish_policy":   types.StringType,
		"replication_mode": types.StringType,
	}}
}

func cursorRowType() attr.Type {
	return types.ObjectType{AttrTypes: map[string]attr.Type{
		"peer":        types.StringType,
		"origin":      types.StringType,
		"applied_seq": types.Int64Type,
		"durable_seq": types.Int64Type,
		"last_error":  types.StringType,
	}}
}

// feedDataFrom fills the single-feed data source. A data source has no prior
// state to preserve, so unlike the resource it simply reports what is there,
// zero values included.
func feedDataFrom(ctx context.Context, feed client.Feed, model *feedDataModel) diag.Diagnostics {
	var diags diag.Diagnostics

	model.Name = types.StringValue(feed.Name)
	model.Format = types.StringValue(feed.Format)
	model.Upstream = types.StringValue(feed.Upstream)
	model.Anonymous = types.BoolValue(feed.Anonymous)
	model.Hosted = types.BoolValue(feed.Hosted)
	model.UpstreamRPS = types.Float64Value(feed.UpstreamRPS)
	model.Redirect = types.BoolValue(feed.Redirect)
	model.RedirectTTL = types.StringValue(feed.RedirectTTL)
	model.PublishPolicy = types.StringValue(feed.PublishPolicy)
	model.ReplicationMode = types.StringValue(feed.ReplicationMode)
	model.PeerFallback = types.BoolValue(feed.PeerFallback)

	publishers, d := types.ListValueFrom(ctx, types.StringType, feed.Publishers)
	diags.Append(d...)
	model.Publishers = publishers

	members, d := types.ListValueFrom(ctx, types.StringType, feed.Members)
	diags.Append(d...)
	model.Members = members

	names := make([]string, 0, len(feed.Policies))
	for _, policy := range feed.Policies {
		names = append(names, policy.Name)
	}
	policies, d := types.ListValueFrom(ctx, types.StringType, names)
	diags.Append(d...)
	model.Policies = policies

	return diags
}
