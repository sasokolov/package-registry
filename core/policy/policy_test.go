package policy

import (
	"context"
	"testing"

	"github.com/sasokolov/package-registry/core/api"
	"github.com/sasokolov/package-registry/core/config"

	_ "github.com/sasokolov/package-registry/policies/allowlist" // register reference policy
)

// scripted is a test policy with a fixed verdict that records invocations.
type scripted struct {
	verdict api.Decision
	calls   *[]string
	label   string
}

func (s scripted) note(hook string) {
	if s.calls != nil {
		*s.calls = append(*s.calls, s.label+"."+hook)
	}
}
func (s scripted) OnResolve(context.Context, api.Identity, api.PackageCoordinate) api.Decision {
	s.note("resolve")
	return s.verdict
}
func (s scripted) OnServe(context.Context, api.Identity, api.Artifact) api.Decision {
	s.note("serve")
	return s.verdict
}
func (s scripted) OnPublish(context.Context, api.Identity, api.Artifact) api.Decision {
	s.note("publish")
	return s.verdict
}

func register(t *testing.T, name string, p api.Policy) {
	t.Helper()
	api.RegisterPolicy(name, func(map[string]any, api.PolicyServices) (api.Policy, error) { return p, nil })
}

func TestEmptyChainAllows(t *testing.T) {
	c, err := NewChain(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := c.OnResolve(t.Context(), api.Anonymous(), api.PackageCoordinate{}); !d.Allow {
		t.Error("empty chain must allow")
	}
}

func TestFirstDenyWinsAndStopsChain(t *testing.T) {
	var calls []string
	register(t, "t-allow-1", scripted{verdict: api.Allowed(), calls: &calls, label: "a1"})
	register(t, "t-deny", scripted{verdict: api.Denied("code-x", "denied by test"), calls: &calls, label: "deny"})
	register(t, "t-allow-2", scripted{verdict: api.Allowed(), calls: &calls, label: "a2"})

	c, err := NewChain([]config.PolicyConfig{{Name: "t-allow-1"}, {Name: "t-deny"}, {Name: "t-allow-2"}}, nil)
	if err != nil {
		t.Fatal(err)
	}

	d := c.OnResolve(t.Context(), api.Anonymous(), api.PackageCoordinate{Name: "x"})
	if d.Allow {
		t.Fatal("deny in chain did not win")
	}
	if d.Policy != "t-deny" || d.Code != "code-x" {
		t.Errorf("decision attribution = %+v", d)
	}
	if len(calls) != 2 || calls[0] != "a1.resolve" || calls[1] != "deny.resolve" {
		t.Errorf("call order = %v, want [a1.resolve deny.resolve] (a2 must not run)", calls)
	}
}

func TestUnknownPolicyFailsChainBuild(t *testing.T) {
	if _, err := NewChain([]config.PolicyConfig{{Name: "does-not-exist"}}, nil); err == nil {
		t.Fatal("NewChain with unknown policy succeeded")
	}
}

func TestAllowlistReferencePolicy(t *testing.T) {
	ctx := t.Context()
	mk := func(allow ...any) *Chain {
		t.Helper()
		c, err := NewChain([]config.PolicyConfig{{
			Name:    "allowlist",
			Options: map[string]any{"allow": allow},
		}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	c := mk("org.apache.*", "@scope/*", "exact:artifact")
	tests := []struct {
		name  string
		allow bool
	}{
		{"org.apache.commons", true},
		{"org.apachefoo", false}, // '.' in the pattern is literal
		{"org.slf4j", false},
		{"@scope/pkg", true},
		{"@other/pkg", false},
		{"@scope/a/b", false}, // '*' must not cross '/'
		{"exact:artifact", true},
	}
	for _, tt := range tests {
		d := c.OnResolve(ctx, api.Anonymous(), api.PackageCoordinate{Format: "test", Name: tt.name})
		if d.Allow != tt.allow {
			t.Errorf("%q: allow = %v, want %v", tt.name, d.Allow, tt.allow)
		}
		if !d.Allow && d.Code != "not-in-allowlist" {
			t.Errorf("%q: code = %q", tt.name, d.Code)
		}
	}

	// OnServe applies the same matching to the artifact coordinate.
	d := c.OnServe(ctx, api.Anonymous(), api.Artifact{Coord: api.PackageCoordinate{Name: "org.slf4j"}})
	if d.Allow {
		t.Error("OnServe allowed a non-allowlisted coordinate")
	}

	// Empty list denies everything.
	empty := mk()
	if d := empty.OnResolve(ctx, api.Anonymous(), api.PackageCoordinate{Name: "anything"}); d.Allow {
		t.Error("empty allowlist must deny")
	}
}

func TestAllowlistBadOptions(t *testing.T) {
	bad := []map[string]any{
		{},                                  // missing allow
		{"allow": "not-a-list"},             // wrong type
		{"allow": []any{42}},                // non-string item
		{"allow": []any{""}},                // empty pattern
		{"allow": []any{"[unclosed-class"}}, // malformed glob
	}
	for i, opts := range bad {
		if _, err := api.NewPolicy("allowlist", opts, nil); err == nil {
			t.Errorf("case %d: NewPolicy accepted bad options %v", i, opts)
		}
	}
}
