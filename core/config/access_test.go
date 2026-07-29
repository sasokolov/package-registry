package config

import (
	"strings"
	"testing"

	"github.com/fondaco-dev/fondaco/core/access"
)

func compiled(t *testing.T, c *Config) *access.Engine {
	t.Helper()
	e, err := c.AccessEngine()
	if err != nil {
		t.Fatalf("AccessEngine: %v", err)
	}
	return e
}

func token(name string) access.Identity {
	return access.Identity{Kind: "token", Subject: name}
}

func anonymous() access.Identity {
	return access.Identity{Kind: "anonymous", Subject: "anonymous"}
}

// The older fields are not a separate permission system any more; they are
// this one, spelled differently. What they used to mean has to survive that
// translation exactly, or an upgrade silently changes who can do what.
func TestLegacyFieldsCompileToWhatTheyAlwaysMeant(t *testing.T) {
	c := &Config{
		Feeds: []FeedConfig{
			{Name: "public", Anonymous: true},
			{Name: "internal", Anonymous: false},
			{Name: "releases", Anonymous: true, Publishers: []string{"token:ci-*"}},
		},
		Admins: []string{"token:ops-*"},
	}
	e := compiled(t, c)

	tests := []struct {
		name string
		id   access.Identity
		path string
		want access.Capability
		ok   bool
	}{
		{name: "an anonymous feed is world-readable", id: anonymous(), path: "feed/public/maven:a", want: access.CapRead, ok: true},
		{name: "a non-anonymous feed is not", id: anonymous(), path: "feed/internal/maven:a", want: access.CapRead},
		{name: "any credential opens a non-anonymous feed", id: token("someone"), path: "feed/internal/maven:a", want: access.CapRead, ok: true},
		{name: "a publisher publishes", id: token("ci-frontend"), path: "feed/releases/maven:a", want: access.CapPublish, ok: true},
		{name: "a non-publisher does not", id: token("dev-laptop"), path: "feed/releases/maven:a", want: access.CapPublish},
		{name: "nobody publishes where there are no publishers", id: token("ci-frontend"), path: "feed/public/maven:a", want: access.CapPublish},
		{name: "an administrator changes the configuration", id: token("ops-oncall"), path: "sys/config", want: access.CapUpdate, ok: true},
		{name: "an ordinary token does not", id: token("dev-laptop"), path: "sys/config", want: access.CapUpdate},
		{name: "an ordinary token cannot even read it", id: token("dev-laptop"), path: "sys/config", want: access.CapRead},
		{name: "but can read operational status", id: token("dev-laptop"), path: "sys/status", want: access.CapRead, ok: true},
		{name: "a stranger cannot", id: anonymous(), path: "sys/status", want: access.CapRead},
		{name: "an administrator issues tokens", id: token("ops-oncall"), path: "sys/tokens", want: access.CapCreate, ok: true},
		{name: "an ordinary token cannot list them", id: token("dev-laptop"), path: "sys/tokens", want: access.CapRead},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := e.Explain(tc.id, tc.path, tc.want)
			if d.Allowed != tc.ok {
				t.Errorf("Allowed = %v, want %v (%s)", d.Allowed, tc.ok, d.Reason)
			}
		})
	}
}

// The generated policies have to meet each other at the same specificity, or
// the narrower one silently caps the broader one — an administrator losing
// the ability to quarantine because a read grant named that path exactly.
func TestGeneratedPoliciesDoNotCancelEachOtherOut(t *testing.T) {
	c := &Config{
		Feeds:  []FeedConfig{{Name: "f", Anonymous: true}},
		Admins: []string{"token:ops-*"},
	}
	e := compiled(t, c)

	for _, area := range operationalAreas() {
		for _, want := range []access.Capability{access.CapRead, access.CapUpdate, access.CapDelete} {
			if !e.Allowed(token("ops-oncall"), area, want) {
				d := e.Explain(token("ops-oncall"), area, want)
				t.Errorf("an administrator cannot %s %s: %s", want, area, d.Reason)
			}
		}
	}
}

// A hand-written policy adds to the generated ones rather than replacing
// them, and can carve exceptions out of what they grant.
func TestWrittenPoliciesComposeWithGeneratedOnes(t *testing.T) {
	c := &Config{
		Feeds: []FeedConfig{{Name: "hosted", Anonymous: true, Publishers: []string{"token:ci-*"}}},
		AccessPolicies: []AccessPolicyConfig{{
			Name: "no-secrets",
			Rules: []AccessRuleConfig{{
				Path:         "feed/hosted/maven:com.secret:*",
				Capabilities: []string{"deny"},
			}},
		}},
		Bindings: []BindingConfig{{
			Policies: []string{"no-secrets"},
			Match:    MatchConfig{Kind: "token", Subject: "ci-*"},
		}},
	}
	e := compiled(t, c)

	if !e.Allowed(token("ci-a"), "feed/hosted/maven:com.example:lib", access.CapPublish) {
		t.Error("the generated publish grant stopped working")
	}
	if e.Allowed(token("ci-a"), "feed/hosted/maven:com.secret:lib", access.CapPublish) {
		t.Error("the written deny did not carve its exception")
	}
	// A deny refuses the path, not one capability on it: reading a package
	// you may not publish is still reaching it, and a rule that meant
	// "publish is denied but read is fine" would have to say so by granting
	// read at a narrower path.
	if e.Allowed(token("ci-a"), "feed/hosted/maven:com.secret:lib", access.CapRead) {
		t.Error("a deny left the path readable")
	}
	// And it applies only to the identities it was bound to.
	if !e.Allowed(token("dev-laptop"), "feed/hosted/maven:com.secret:lib", access.CapRead) {
		t.Error("the deny leaked to an identity its binding does not select")
	}
}

func TestReservedPolicyNamesAreRefused(t *testing.T) {
	c := &Config{
		Site:    SiteConfig{Name: "test"},
		Server:  ServerConfig{Listen: ":8080"},
		Storage: StorageConfig{Type: StorageFS, FS: FSConfig{Path: "/tmp/x"}},
		AccessPolicies: []AccessPolicyConfig{{
			Name:  "feed:hosted:read",
			Rules: []AccessRuleConfig{{Path: "feed/x/*", Capabilities: []string{"read"}}},
		}},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("a reserved policy name was accepted: %v", err)
	}
}

func TestAccessValidationCatchesMistakes(t *testing.T) {
	base := func(policies []AccessPolicyConfig, bindings []BindingConfig) *Config {
		return &Config{
			Site:           SiteConfig{Name: "test"},
			Server:         ServerConfig{Listen: ":8080"},
			Storage:        StorageConfig{Type: StorageFS, FS: FSConfig{Path: "/tmp/x"}},
			AccessPolicies: policies,
			Bindings:       bindings,
		}
	}

	tests := []struct {
		name   string
		config *Config
		want   string
	}{
		{
			name: "a path in no namespace",
			config: base([]AccessPolicyConfig{{
				Name: "p", Rules: []AccessRuleConfig{{Path: "secret/x", Capabilities: []string{"read"}}},
			}}, nil),
			want: "must start with",
		},
		{
			name: "a star in the middle",
			config: base([]AccessPolicyConfig{{
				Name: "p", Rules: []AccessRuleConfig{{Path: "feed/*/x", Capabilities: []string{"read"}}},
			}}, nil),
			want: "only allowed at the end",
		},
		{
			name: "a capability that does not exist",
			config: base([]AccessPolicyConfig{{
				Name: "p", Rules: []AccessRuleConfig{{Path: "feed/x/*", Capabilities: []string{"sudo"}}},
			}}, nil),
			want: "not a capability",
		},
		{
			name: "a binding to a policy nobody wrote",
			config: base([]AccessPolicyConfig{{
				Name: "p", Rules: []AccessRuleConfig{{Path: "feed/x/*", Capabilities: []string{"read"}}},
			}}, []BindingConfig{{Policies: []string{"ghost"}}}),
			want: "is not defined",
		},
		{
			name: "an identity kind that does not exist",
			config: base([]AccessPolicyConfig{{
				Name: "p", Rules: []AccessRuleConfig{{Path: "feed/x/*", Capabilities: []string{"read"}}},
			}}, []BindingConfig{{Policies: []string{"p"}, Match: MatchConfig{Kind: "ldap"}}}),
			want: "not an identity kind",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.config.Validate()
			if err == nil {
				t.Fatalf("accepted a configuration that cannot be right; wanted %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}
