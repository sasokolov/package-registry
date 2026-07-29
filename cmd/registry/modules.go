package main

// Module set of the production binary (compile-time assembly, Caddy style).
// Format modules arrive in Phase 2+; the conformance-only echo module is
// linked via modules_conformance.go behind the "conformance" build tag.
import (
	_ "github.com/sasokolov/package-registry/modules/format/composer"
	_ "github.com/sasokolov/package-registry/modules/format/helm"
	_ "github.com/sasokolov/package-registry/modules/format/maven"
	_ "github.com/sasokolov/package-registry/modules/format/npm"
	_ "github.com/sasokolov/package-registry/modules/format/nuget"
	_ "github.com/sasokolov/package-registry/modules/format/terraform"
	_ "github.com/sasokolov/package-registry/modules/storage/fs"
	_ "github.com/sasokolov/package-registry/modules/storage/s3"
	_ "github.com/sasokolov/package-registry/policies/allowlist"
	_ "github.com/sasokolov/package-registry/policies/license"
	_ "github.com/sasokolov/package-registry/policies/osv"
	_ "github.com/sasokolov/package-registry/policies/quarantine"
)
