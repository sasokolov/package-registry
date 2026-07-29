package main

// Module set of the production binary (compile-time assembly, Caddy style).
// Format modules arrive in Phase 2+; the conformance-only echo module is
// linked via modules_conformance.go behind the "conformance" build tag.
import (
	_ "github.com/fondaco-dev/fondaco/modules/format/composer"
	_ "github.com/fondaco-dev/fondaco/modules/format/helm"
	_ "github.com/fondaco-dev/fondaco/modules/format/maven"
	_ "github.com/fondaco-dev/fondaco/modules/format/npm"
	_ "github.com/fondaco-dev/fondaco/modules/format/nuget"
	_ "github.com/fondaco-dev/fondaco/modules/format/oci"
	_ "github.com/fondaco-dev/fondaco/modules/format/terraform"
	_ "github.com/fondaco-dev/fondaco/modules/storage/fs"
	_ "github.com/fondaco-dev/fondaco/modules/storage/s3"
	_ "github.com/fondaco-dev/fondaco/policies/allowlist"
	_ "github.com/fondaco-dev/fondaco/policies/license"
	_ "github.com/fondaco-dev/fondaco/policies/osv"
	_ "github.com/fondaco-dev/fondaco/policies/quarantine"
)
