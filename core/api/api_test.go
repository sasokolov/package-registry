package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

type fakeModule struct{ name string }

func (m fakeModule) Name() string    { return m.name }
func (m fakeModule) Routes() []Route { return nil }
func (m fakeModule) Parse(*http.Request) (Intent, error) {
	return Intent{}, nil
}
func (m fakeModule) RewriteMetadata(Feed, []byte) ([]byte, error) { return nil, nil }

func TestFormatRegistry(t *testing.T) {
	RegisterFormat(fakeModule{name: "fmt-test"})
	if _, ok := Format("fmt-test"); !ok {
		t.Fatal("registered format not found")
	}
	if _, ok := Format("missing"); ok {
		t.Fatal("unregistered format found")
	}

	defer func() {
		if recover() == nil {
			t.Fatal("duplicate registration did not panic")
		}
	}()
	RegisterFormat(fakeModule{name: "fmt-test"})
}

type allowAll struct{}

func (allowAll) OnResolve(context.Context, Identity, PackageCoordinate) Decision { return Allowed() }
func (allowAll) OnServe(context.Context, Identity, Artifact) Decision            { return Allowed() }
func (allowAll) OnPublish(context.Context, Identity, Artifact) Decision          { return Allowed() }

func TestPolicyRegistry(t *testing.T) {
	RegisterPolicy("pol-test", func(map[string]any, PolicyServices) (Policy, error) { return allowAll{}, nil })
	if !PolicyRegistered("pol-test") {
		t.Fatal("registered policy not reported")
	}
	if _, err := NewPolicy("pol-test", nil, nil); err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	if _, err := NewPolicy("missing", nil, nil); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("NewPolicy(missing) = %v, want not-registered error", err)
	}
}

func TestIdentity(t *testing.T) {
	if !Anonymous().IsAnonymous() {
		t.Error("Anonymous() must be anonymous")
	}
	id := Identity{Kind: IdentityToken, Subject: "ci-bot"}
	if id.IsAnonymous() {
		t.Error("token identity must not be anonymous")
	}
	if got := id.String(); got != "token:ci-bot" {
		t.Errorf("String() = %q", got)
	}
}

func TestCoordinateString(t *testing.T) {
	c := PackageCoordinate{Format: "maven", Name: "org.slf4j:slf4j-api", Version: "2.0.13"}
	if got := c.String(); got != "maven:org.slf4j:slf4j-api@2.0.13" {
		t.Errorf("String() = %q", got)
	}
	c.Version = ""
	if got := c.String(); got != "maven:org.slf4j:slf4j-api" {
		t.Errorf("String() without version = %q", got)
	}
}
