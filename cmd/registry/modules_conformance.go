//go:build conformance

package main

// Test-only module set for conformance builds.
import (
	_ "github.com/sasokolov/package-registry/conformance/echomodule"
)
