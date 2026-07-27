package provider

import (
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"
)

// The optional interfaces are what make Configure, ValidateConfig and
// ImportState get called at all. A method with the right name but the wrong
// signature is simply never invoked, which is a silent loss of validation —
// so the compiler is asked to check instead.
var (
	_ provider.Provider = (*registryProvider)(nil)

	_ resource.ResourceWithConfigure      = (*feedResource)(nil)
	_ resource.ResourceWithValidateConfig = (*feedResource)(nil)
	_ resource.ResourceWithImportState    = (*feedResource)(nil)

	_ resource.ResourceWithConfigure   = (*adminBindingResource)(nil)
	_ resource.ResourceWithImportState = (*adminBindingResource)(nil)

	_ resource.ResourceWithConfigure      = (*oidcIssuerResource)(nil)
	_ resource.ResourceWithValidateConfig = (*oidcIssuerResource)(nil)
	_ resource.ResourceWithImportState    = (*oidcIssuerResource)(nil)

	_ resource.ResourceWithConfigure      = (*replicationPeerResource)(nil)
	_ resource.ResourceWithValidateConfig = (*replicationPeerResource)(nil)
	_ resource.ResourceWithImportState    = (*replicationPeerResource)(nil)

	_ resource.ResourceWithConfigure = (*tokenResource)(nil)

	_ resource.ResourceWithConfigure      = (*quarantineResource)(nil)
	_ resource.ResourceWithValidateConfig = (*quarantineResource)(nil)
	_ resource.ResourceWithImportState    = (*quarantineResource)(nil)

	_ datasource.DataSourceWithConfigure = (*siteDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*feedDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*feedsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*replicationStatusDataSource)(nil)
)
