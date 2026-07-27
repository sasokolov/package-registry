// Command terraform-provider-registry serves the Terraform provider for the
// package registry.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/sasokolov/package-registry/terraform-provider-registry/internal/provider"
)

// version is set by the release build; "dev" is what a local build reports.
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false,
		"run with support for debuggers, printing a TF_REATTACH_PROVIDERS line to attach to")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: "registry.local/sasokolov/registry",
		Debug:   debug,
	})
	if err != nil {
		log.Fatal(err)
	}
}
