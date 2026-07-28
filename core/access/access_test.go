package access

import (
	"strings"
	"testing"
)

func engine(t *testing.T, policies []Policy, bindings []Binding) *Engine {
	t.Helper()
	e, err := New(policies, bindings)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func ci() Identity {
	return Identity{Kind: "token", Subject: "ci-frontend"}
}

func anonymous() Identity { return Identity{Kind: "anonymous", Subject: "anonymous"} }

// Nothing is allowed until something says so.
func TestDefaultIsRefusal(t *testing.T) {
	e := engine(t, nil, nil)
	d := e.Explain(ci(), "feed/releases/maven:com.example:lib", CapRead)
	if d.Allowed {
		t.Fatal("an empty configuration allowed a read")
	}
	if !strings.Contains(d.Reason, "no policy") {
		t.Errorf("reason = %q", d.Reason)
	}
}

func TestGrantAndRefusal(t *testing.T) {
	e := engine(t,
		[]Policy{{Name: "readonly", Rules: []Rule{
			{Path: "feed/releases/*", Capabilities: Capabilities{CapRead, CapList}},
		}}},
		[]Binding{{Policies: []string{"readonly"}, Match: Match{Kind: "token"}}},
	)

	tests := []struct {
		name string
		path string
		want Capability
		ok   bool
	}{
		{name: "granted capability on a granted path", path: "feed/releases/maven:x", want: CapRead, ok: true},
		{name: "a capability the rule does not grant", path: "feed/releases/maven:x", want: CapPublish},
		{name: "a path no rule covers", path: "feed/private/maven:x", want: CapRead},
		{name: "a sys path is not covered by a feed rule", path: "sys/config", want: CapRead},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := e.Allowed(ci(), tc.path, tc.want); got != tc.ok {
				t.Errorf("Allowed(%q, %s) = %v, want %v", tc.path, tc.want, got, tc.ok)
			}
		})
	}
}

// The point of an explicit deny: forbid a whole area, then allow exactly one
// thing inside it. If the broad deny won regardless of specificity, nobody
// could express an exception and denies would go unused.
func TestNarrowGrantIsAnExceptionToABroadDeny(t *testing.T) {
	e := engine(t,
		[]Policy{{Name: "guarded", Rules: []Rule{
			{Path: "feed/private/*", Capabilities: Capabilities{CapDeny}},
			{Path: "feed/private/maven:com.example:*", Capabilities: Capabilities{CapRead}},
		}}},
		[]Binding{{Policies: []string{"guarded"}, Match: Match{}}},
	)

	if !e.Allowed(ci(), "feed/private/maven:com.example:lib@1.0.0", CapRead) {
		t.Error("the narrow grant did not override the broad deny")
	}
	if e.Allowed(ci(), "feed/private/maven:com.acme:lib@1.0.0", CapRead) {
		t.Error("the broad deny did not apply outside the exception")
	}
}

// And the other direction: a narrow deny carves a hole in a broad grant.
func TestNarrowDenyIsAnExceptionToABroadGrant(t *testing.T) {
	e := engine(t,
		[]Policy{{Name: "mostly", Rules: []Rule{
			{Path: "feed/releases/*", Capabilities: Capabilities{CapRead}},
			{Path: "feed/releases/maven:com.secret:*", Capabilities: Capabilities{CapDeny}},
		}}},
		[]Binding{{Policies: []string{"mostly"}, Match: Match{}}},
	)

	if !e.Allowed(ci(), "feed/releases/maven:com.example:lib", CapRead) {
		t.Error("the broad grant stopped working")
	}
	if e.Allowed(ci(), "feed/releases/maven:com.secret:lib", CapRead) {
		t.Error("the narrow deny did not apply")
	}
}

// On the same path, a deny beats a grant however it arrived — including
// from a different policy.
func TestDenyBeatsAGrantAtTheSameSpecificity(t *testing.T) {
	e := engine(t,
		[]Policy{
			{Name: "grant", Rules: []Rule{{Path: "feed/x/*", Capabilities: Capabilities{CapRead, CapPublish}}}},
			{Name: "revoke", Rules: []Rule{{Path: "feed/x/*", Capabilities: Capabilities{CapDeny}}}},
		},
		[]Binding{{Policies: []string{"grant", "revoke"}, Match: Match{}}},
	)

	d := e.Explain(ci(), "feed/x/maven:a", CapRead)
	if d.Allowed {
		t.Fatal("a deny lost to a grant on the same path")
	}
	if d.Policy != "revoke" {
		t.Errorf("the explanation blames %q, want the policy that denied", d.Policy)
	}
	if !strings.Contains(d.Reason, "denies") {
		t.Errorf("reason = %q", d.Reason)
	}
}

// Two policies granting different things on the same path add up, which is
// what makes small reusable policies worth having.
func TestGrantsFromSeveralPoliciesCombine(t *testing.T) {
	e := engine(t,
		[]Policy{
			{Name: "reader", Rules: []Rule{{Path: "feed/x/*", Capabilities: Capabilities{CapRead}}}},
			{Name: "publisher", Rules: []Rule{{Path: "feed/x/*", Capabilities: Capabilities{CapPublish}}}},
		},
		[]Binding{{Policies: []string{"reader", "publisher"}, Match: Match{}}},
	)
	for _, want := range []Capability{CapRead, CapPublish} {
		if !e.Allowed(ci(), "feed/x/maven:a", want) {
			t.Errorf("%s was not granted", want)
		}
	}
}

// Bindings are the mapping from what the auth system said to what may be
// done, so every claim has to be able to carry a condition.
func TestBindingsMatchAuthenticationData(t *testing.T) {
	policies := []Policy{{Name: "p", Rules: []Rule{
		{Path: "feed/x/*", Capabilities: Capabilities{CapPublish}},
	}}}

	tests := []struct {
		name  string
		match Match
		id    Identity
		bound bool
	}{
		{
			name:  "a token name glob",
			match: Match{Kind: "token", Subject: "ci-*"},
			id:    Identity{Kind: "token", Subject: "ci-frontend"},
			bound: true,
		},
		{
			name:  "a token that does not match the glob",
			match: Match{Kind: "token", Subject: "ci-*"},
			id:    Identity{Kind: "token", Subject: "dev-laptop"},
		},
		{
			name:  "a GitLab project path",
			match: Match{Kind: "oidc", ProjectPath: "platform/*"},
			id:    Identity{Kind: "oidc", Subject: "sub", ProjectPath: "platform/api"},
			bound: true,
		},
		{
			name:  "a branch condition",
			match: Match{Kind: "oidc", ProjectPath: "platform/*", Ref: "refs/heads/main"},
			id:    Identity{Kind: "oidc", ProjectPath: "platform/api", Ref: "refs/heads/feature"},
		},
		{
			name:  "the issuer has to match too",
			match: Match{Kind: "oidc", Issuer: "https://gitlab.example.com"},
			id:    Identity{Kind: "oidc", Issuer: "https://evil.example.com"},
		},
		{
			name:  "any authenticated identity",
			match: Match{Authenticated: true},
			id:    Identity{Kind: "token", Subject: "anyone"},
			bound: true,
		},
		{
			name:  "authenticated excludes anonymous",
			match: Match{Authenticated: true},
			id:    anonymous(),
		},
		{
			name:  "no conditions binds everyone, anonymous included",
			match: Match{},
			id:    anonymous(),
			bound: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := engine(t, policies, []Binding{{Policies: []string{"p"}, Match: tc.match}})
			if got := e.Allowed(tc.id, "feed/x/maven:a", CapPublish); got != tc.bound {
				t.Errorf("Allowed = %v, want %v", got, tc.bound)
			}
		})
	}
}

// A refusal nobody can explain is a refusal people route around.
func TestExplanationsNameTheDecidingRule(t *testing.T) {
	e := engine(t,
		[]Policy{{Name: "team", Rules: []Rule{
			{Path: "feed/releases/*", Capabilities: Capabilities{CapRead}},
		}}},
		[]Binding{{Policies: []string{"team"}, Match: Match{}}},
	)

	granted := e.Explain(ci(), "feed/releases/maven:a", CapRead)
	if granted.Policy != "team" || granted.Rule != "feed/releases/*" {
		t.Errorf("granted explanation = %+v", granted)
	}
	if !strings.Contains(granted.Reason, "grants read") {
		t.Errorf("reason = %q", granted.Reason)
	}

	refused := e.Explain(ci(), "feed/releases/maven:a", CapPublish)
	if refused.Allowed {
		t.Fatal("publish was allowed")
	}
	if !strings.Contains(refused.Reason, "does not include publish") {
		t.Errorf("reason = %q, want it to say what was granted instead", refused.Reason)
	}
	if !refused.Capabilities.Has(CapRead) {
		t.Errorf("the explanation does not report the effective capabilities: %+v", refused)
	}
}

func TestConfigurationMistakesAreRefusedAtCompileTime(t *testing.T) {
	tests := []struct {
		name     string
		policies []Policy
		bindings []Binding
		want     string
	}{
		{
			name:     "a policy without a name",
			policies: []Policy{{Rules: []Rule{{Path: "feed/x/*", Capabilities: Capabilities{CapRead}}}}},
			want:     "no name",
		},
		{
			name: "two policies with one name",
			policies: []Policy{
				{Name: "p", Rules: []Rule{{Path: "a", Capabilities: Capabilities{CapRead}}}},
				{Name: "p", Rules: []Rule{{Path: "b", Capabilities: Capabilities{CapRead}}}},
			},
			want: "defined twice",
		},
		{
			name:     "a capability that does not exist",
			policies: []Policy{{Name: "p", Rules: []Rule{{Path: "a", Capabilities: Capabilities{"sudo"}}}}},
			want:     "not a capability",
		},
		{
			name:     "a star in the middle",
			policies: []Policy{{Name: "p", Rules: []Rule{{Path: "feed/*/x", Capabilities: Capabilities{CapRead}}}}},
			want:     `only allowed at the end`,
		},
		{
			name:     "a rule that grants nothing",
			policies: []Policy{{Name: "p", Rules: []Rule{{Path: "a"}}}},
			want:     "grants nothing",
		},
		{
			name:     "a binding to a policy that does not exist",
			policies: []Policy{{Name: "p", Rules: []Rule{{Path: "a", Capabilities: Capabilities{CapRead}}}}},
			bindings: []Binding{{Policies: []string{"ghost"}}},
			want:     "does not exist",
		},
		{
			name:     "a binding that attaches nothing",
			policies: []Policy{{Name: "p", Rules: []Rule{{Path: "a", Capabilities: Capabilities{CapRead}}}}},
			bindings: []Binding{{Match: Match{Kind: "token"}}},
			want:     "attaches no policies",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.policies, tc.bindings)
			if err == nil {
				t.Fatalf("accepted a configuration that cannot be right; wanted %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// "No policy applies to you" and "the policy that applies grants too little"
// are different faults with different fixes, and the second one is the only
// one people think to look for. Naming the bindings that matched separates
// them at a glance.
func TestExplanationsNameTheBindingsThatMatched(t *testing.T) {
	e := engine(t,
		[]Policy{
			{Name: "team", Rules: []Rule{
				{Path: "feed/releases/*", Capabilities: Capabilities{CapRead}},
			}},
			{Name: "oncall", Rules: []Rule{
				{Path: "sys/quarantine", Capabilities: Capabilities{CapUpdate}},
			}},
		},
		[]Binding{
			{Name: "frontend-ci", Policies: []string{"team"}, Match: Match{Subject: "ci-*"}},
			{Name: "rotation", Policies: []string{"oncall"}, Match: Match{Subject: "ci-frontend"}},
			{Name: "someone-else", Policies: []string{"team"}, Match: Match{Subject: "release-bot"}},
		},
	)

	d := e.Explain(ci(), "feed/releases/maven:a", CapRead)
	if len(d.Bindings) != 2 || d.Bindings[0] != "frontend-ci" || d.Bindings[1] != "rotation" {
		t.Errorf("bindings = %v, want the two that matched, in order", d.Bindings)
	}

	// A binding that did not match must not appear, or the field would be a
	// list of everything and worth nothing.
	for _, name := range d.Bindings {
		if name == "someone-else" {
			t.Errorf("a binding that does not match this identity was reported: %v", d.Bindings)
		}
	}

	// Nothing bound at all is the case the field exists for.
	stranger := e.Explain(Identity{Kind: "token", Subject: "nobody"},
		"feed/releases/maven:a", CapRead)
	if len(stranger.Bindings) != 0 {
		t.Errorf("bindings = %v, want none", stranger.Bindings)
	}
	if !strings.Contains(stranger.Reason, "no policy is bound") {
		t.Errorf("reason = %q", stranger.Reason)
	}
}

// The compiled-in bindings have no name; reporting them as an empty string
// would put a nameless entry in the list and make "matched nothing" and
// "matched the generated one" indistinguishable.
func TestGeneratedBindingsAreNotNamed(t *testing.T) {
	e := engine(t,
		[]Policy{{Name: "public", Rules: []Rule{
			{Path: "feed/central/*", Capabilities: Capabilities{CapRead}},
		}}},
		[]Binding{{Policies: []string{"public"}}},
	)

	d := e.Explain(anonymous(), "feed/central/maven:a", CapRead)
	if !d.Allowed {
		t.Fatalf("anonymous read was refused: %+v", d)
	}
	if len(d.Bindings) != 0 {
		t.Errorf("bindings = %v, want none for a binding with no name", d.Bindings)
	}
	if len(d.Policies) != 1 || d.Policies[0] != "public" {
		t.Errorf("policies = %v", d.Policies)
	}
}
