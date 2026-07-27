package provider

import (
	"context"
	"errors"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"

	"github.com/sasokolov/package-registry/terraform-provider-registry/internal/client"
)

// pathRoot is a shorthand for the provider-level attribute paths.
func pathRoot(name string) path.Path { return path.Root(name) }

// configureResource pulls the shared client out of the provider data. The
// nil case is normal: the framework calls Configure with no data during
// validation, before the provider itself is configured.
func configureResource(req resource.ConfigureRequest, resp *resource.ConfigureResponse) *client.Client {
	if req.ProviderData == nil {
		return nil
	}
	c, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected provider data",
			"The provider handed the resource something that is not a registry client. This is a bug in the provider.",
		)
		return nil
	}
	return c
}

// configureDataSource is configureResource for data sources.
func configureDataSource(req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) *client.Client {
	if req.ProviderData == nil {
		return nil
	}
	c, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected provider data",
			"The provider handed the data source something that is not a registry client. This is a bug in the provider.",
		)
		return nil
	}
	return c
}

// reportWriteError turns a client error into diagnostics that say what the
// operator can do about it. A conflict and a rejection are different
// situations and reading "500" for both is how a plan becomes a guess.
func reportWriteError(diags *diag.Diagnostics, action string, err error) {
	var apiErr *client.Error
	switch {
	case errors.As(err, &apiErr) && apiErr.Conflict():
		diags.AddError(
			"The registry rejected "+action+" as a conflict",
			apiErr.Message+"\n\nAnother writer changed the configuration at the same time. "+
				"Re-run the apply; the registry serializes writes, so the retry sees the newer document.",
		)
	case errors.As(err, &apiErr) && apiErr.Invalid():
		diags.AddError(
			"The registry refused "+action+" as invalid",
			apiErr.Message+"\n\nThe configuration was validated as a whole before it was stored, "+
				"so nothing was changed.",
		)
	case errors.As(err, &apiErr) && apiErr.Status == 403:
		diags.AddError(
			"Not allowed to perform "+action,
			apiErr.Message+"\n\nThe provider's identity must match one of the site's admins patterns.",
		)
	default:
		diags.AddError("Failed to perform "+action, err.Error())
	}
}

// removedOutsideTerraform reports whether an error means the resource is gone
// and, if so, drops it from state.
func removedOutsideTerraform(ctx context.Context, err error, state *resource.ReadResponse) bool {
	if errors.Is(err, client.ErrNotFound) {
		state.State.RemoveResource(ctx)
		return true
	}
	return false
}

// pathOf builds an attribute path for a diagnostic.
func pathOf(name string) path.Path { return path.Root(name) }
