package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sasokolov/package-registry/core/access"
)

// Access control as configuration.
//
// The declarative form is the whole story: policies and bindings live in the
// same document as everything else (invariant 8), so who may do what is
// reviewed, versioned and rolled back exactly like which feeds exist.
//
// The older fields — a feed's anonymous and publishers, the site's admins —
// are kept, and are compiled into policies here. That is deliberate: two
// permission systems with two sets of rules would mean every "why was this
// refused" question has two places to look, and eventually they disagree.
// One engine, one semantics, and the older spelling is sugar over it.

// AccessPolicyConfig is a named set of rules.
type AccessPolicyConfig struct {
	Name  string             `yaml:"name" json:"name"`
	Rules []AccessRuleConfig `yaml:"rules" json:"rules"`
}

// AccessRuleConfig grants capabilities on a path.
type AccessRuleConfig struct {
	Path         string   `yaml:"path" json:"path"`
	Capabilities []string `yaml:"capabilities" json:"capabilities"`
}

// BindingConfig attaches policies to the identities a match selects.
type BindingConfig struct {
	Policies []string    `yaml:"policies" json:"policies"`
	Match    MatchConfig `yaml:"match" json:"match"`
}

// MatchConfig selects identities by what authentication established about
// them. An empty field is not a condition.
type MatchConfig struct {
	// Kind is "token", "oidc" or "anonymous".
	Kind string `yaml:"kind,omitempty" json:"kind,omitempty"`
	// Issuer is the OIDC issuer that vouched for the identity.
	Issuer string `yaml:"issuer,omitempty" json:"issuer,omitempty"`
	// Subject is the token name or the OIDC subject claim; a trailing "*"
	// makes it a prefix.
	Subject string `yaml:"subject,omitempty" json:"subject,omitempty"`
	// ProjectPath and Ref are GitLab CI claims.
	ProjectPath string `yaml:"project_path,omitempty" json:"project_path,omitempty"`
	Ref         string `yaml:"ref,omitempty" json:"ref,omitempty"`
	// Authenticated requires any non-anonymous identity.
	Authenticated bool `yaml:"authenticated,omitempty" json:"authenticated,omitempty"`
}

// Access namespaces. Content lives under feed/, administration under sys/.
const (
	FeedPathPrefix = "feed/"
	SysPathPrefix  = "sys/"
)

// The sys areas, each one an operation an operator asks about by name.
const (
	SysConfig      = "sys/config"
	SysTokens      = "sys/tokens"
	SysQuarantine  = "sys/quarantine"
	SysConflicts   = "sys/conflicts"
	SysReplication = "sys/replication"
	SysStatus      = "sys/status"
	SysFeeds       = "sys/feeds"
)

// FeedPath is the access path of a coordinate in a feed. An empty coordinate
// names the feed itself, which is what a listing asks about.
func FeedPath(feed, coordinate string) string {
	if coordinate == "" {
		return FeedPathPrefix + feed
	}
	return FeedPathPrefix + feed + "/" + coordinate
}

// AccessEngine compiles the document's access rules — the explicit policies
// and the ones implied by the older fields — into one engine.
func (c *Config) AccessEngine() (*access.Engine, error) {
	policies, bindings := c.compileLegacy()

	for _, p := range c.AccessPolicies {
		rules := make([]access.Rule, 0, len(p.Rules))
		for _, r := range p.Rules {
			caps := make(access.Capabilities, 0, len(r.Capabilities))
			for _, raw := range r.Capabilities {
				caps = append(caps, access.Capability(raw))
			}
			rules = append(rules, access.Rule{Path: r.Path, Capabilities: caps})
		}
		policies = append(policies, access.Policy{Name: p.Name, Rules: rules})
	}
	for _, b := range c.Bindings {
		bindings = append(bindings, access.Binding{
			Policies: b.Policies,
			Match: access.Match{
				Kind:          b.Match.Kind,
				Issuer:        b.Match.Issuer,
				Subject:       b.Match.Subject,
				ProjectPath:   b.Match.ProjectPath,
				Ref:           b.Match.Ref,
				Authenticated: b.Match.Authenticated,
			},
		})
	}

	return access.New(policies, bindings)
}

// compileLegacy turns anonymous, publishers and admins into policies. The
// generated names are prefixed so they are recognisable in an explanation:
// an operator who sees "feed:releases:read" knows where it came from without
// being told.
func (c *Config) compileLegacy() ([]access.Policy, []access.Binding) {
	var policies []access.Policy
	var bindings []access.Binding

	for _, f := range c.Feeds {
		readName := "feed:" + f.Name + ":read"
		policies = append(policies, access.Policy{
			Name: readName,
			Rules: []access.Rule{{
				Path:         FeedPath(f.Name, "*"),
				Capabilities: access.Capabilities{access.CapRead, access.CapList},
			}},
		})
		// An anonymous feed is readable by everyone; a feed that is not
		// anonymous is readable by anyone who authenticated at all, which is
		// what it has always meant.
		bindings = append(bindings, access.Binding{
			Policies: []string{readName},
			Match:    access.Match{Authenticated: !f.Anonymous},
		})

		if len(f.Publishers) == 0 {
			continue
		}
		publishName := "feed:" + f.Name + ":publish"
		policies = append(policies, access.Policy{
			Name: publishName,
			Rules: []access.Rule{{
				Path:         FeedPath(f.Name, "*"),
				Capabilities: access.Capabilities{access.CapPublish},
			}},
		})
		for _, pattern := range f.Publishers {
			bindings = append(bindings, access.Binding{
				Policies: []string{publishName},
				Match:    matchFromPattern(pattern),
			})
		}
	}

	if len(c.Admins) > 0 {
		everything := access.Capabilities{
			access.CapRead, access.CapList, access.CapCreate,
			access.CapUpdate, access.CapDelete,
		}
		// One catch-all rule plus one per area, and the per-area rules are
		// the point. The most specific matching rule decides, so a rule
		// naming sys/quarantine exactly would otherwise beat this policy's
		// sys/* and quietly cap an administrator at whatever that narrower
		// rule granted. Generated policies have to meet each other at the
		// same specificity or they cancel out.
		rules := []access.Rule{{Path: SysPathPrefix + "*", Capabilities: everything}}
		for _, area := range sysAreas() {
			rules = append(rules, access.Rule{Path: area, Capabilities: everything})
		}
		policies = append(policies, access.Policy{Name: "sys:admin", Rules: rules})
		for _, pattern := range c.Admins {
			bindings = append(bindings, access.Binding{
				Policies: []string{"sys:admin"},
				Match:    matchFromPattern(pattern),
			})
		}
	}

	// Every identified caller may read the operational surface: what this
	// site is, what it serves, how replication is doing, what is blocked.
	// That is what the read API has always allowed.
	//
	// It is enumerated rather than written as sys/*, and the difference
	// matters: sys/config and sys/tokens are not operational status, they
	// are the keys to the building. A blanket grant here would have let any
	// token holder read who may do what — which is most of the value of
	// having written it down.
	operational := make([]access.Rule, 0, 5)
	for _, area := range operationalAreas() {
		operational = append(operational, access.Rule{
			Path:         area,
			Capabilities: access.Capabilities{access.CapRead, access.CapList},
		})
	}
	policies = append(policies, access.Policy{Name: "sys:read", Rules: operational})
	bindings = append(bindings, access.Binding{
		Policies: []string{"sys:read"},
		Match:    access.Match{Authenticated: true},
	})

	return policies, bindings
}

// sysAreas lists every administrative area, so a policy that means "all of
// it" can say so at every specificity a narrower rule might use.
func sysAreas() []string {
	return []string{
		SysConfig, SysTokens, SysQuarantine, SysConflicts,
		SysReplication, SysStatus, SysFeeds,
	}
}

// operationalAreas is what an identified caller may read without being an
// administrator: what the site is and how it is doing, never how it is
// configured or who holds a credential.
func operationalAreas() []string {
	return []string{SysStatus, SysFeeds, SysReplication, SysConflicts, SysQuarantine}
}

// matchFromPattern translates a publisher/admin pattern into a binding.
//
//	token:<name>      a static token, exact or with a trailing *
//	oidc:<sub>        an OIDC subject
//	project:<path>    a GitLab project_path
//	project:<p>@<ref> a project on one ref
//	*                 anyone authenticated
func matchFromPattern(pattern string) access.Match {
	if pattern == "*" {
		return access.Match{Authenticated: true}
	}
	kind, rest, ok := strings.Cut(pattern, ":")
	if !ok {
		// Not a shape this language defines; match nothing rather than
		// guess. Validation rejects it before it reaches here.
		return access.Match{Kind: "none"}
	}
	switch kind {
	case "token", "oidc":
		return access.Match{Kind: kind, Subject: rest}
	case "project":
		project, ref, hasRef := strings.Cut(rest, "@")
		m := access.Match{Kind: "oidc", ProjectPath: project}
		if hasRef {
			m.Ref = ref
		}
		return m
	default:
		return access.Match{Kind: "none"}
	}
}

// validateAccess checks the access section on its own terms, so a mistake is
// reported at load time with the offending line rather than as a refusal
// somebody has to reverse-engineer later.
func (c *Config) validateAccess() []error {
	var errs []error

	names := map[string]bool{}
	for i, p := range c.AccessPolicies {
		at := fmt.Sprintf("access_policies[%d]", i)
		if p.Name != "" {
			at = fmt.Sprintf("access_policies[%d] (%s)", i, p.Name)
		}
		switch {
		case p.Name == "":
			errs = append(errs, fmt.Errorf("%s: name is required", at))
		case strings.HasPrefix(p.Name, "feed:") || strings.HasPrefix(p.Name, "sys:"):
			// Those names are generated from anonymous/publishers/admins;
			// letting a hand-written policy take one would make an
			// explanation name something other than what it describes.
			errs = append(errs, fmt.Errorf(
				"%s: the %q prefix is reserved for policies generated from anonymous/publishers/admins",
				at, strings.SplitN(p.Name, ":", 2)[0]+":"))
		case names[p.Name]:
			errs = append(errs, fmt.Errorf("%s: duplicate policy name", at))
		default:
			names[p.Name] = true
		}

		if len(p.Rules) == 0 {
			errs = append(errs, fmt.Errorf("%s: has no rules", at))
		}
		for j, r := range p.Rules {
			errs = append(errs, validateAccessRule(at, j, r)...)
		}
	}

	for i, b := range c.Bindings {
		at := fmt.Sprintf("bindings[%d]", i)
		if len(b.Policies) == 0 {
			errs = append(errs, fmt.Errorf("%s: names no policies", at))
		}
		for _, name := range b.Policies {
			if !names[name] {
				errs = append(errs, fmt.Errorf("%s: policy %q is not defined", at, name))
			}
		}
		switch b.Match.Kind {
		case "", "token", "oidc", "anonymous":
		default:
			errs = append(errs, fmt.Errorf(
				"%s: match.kind %q is not an identity kind (want token, oidc or anonymous)", at, b.Match.Kind))
		}
	}

	return errs
}

func validateAccessRule(at string, j int, r AccessRuleConfig) []error {
	var errs []error
	switch {
	case r.Path == "":
		errs = append(errs, fmt.Errorf("%s: rules[%d]: path is required", at, j))
	case !strings.HasPrefix(r.Path, FeedPathPrefix) && !strings.HasPrefix(r.Path, SysPathPrefix):
		errs = append(errs, fmt.Errorf(
			"%s: rules[%d]: path %q must start with %q or %q",
			at, j, r.Path, FeedPathPrefix, SysPathPrefix))
	case strings.Contains(strings.TrimSuffix(r.Path, "*"), "*"):
		errs = append(errs, fmt.Errorf(
			"%s: rules[%d]: path %q — \"*\" is only allowed at the end; use \"+\" to match one segment",
			at, j, r.Path))
	}

	if len(r.Capabilities) == 0 {
		errs = append(errs, fmt.Errorf("%s: rules[%d]: grants no capabilities", at, j))
	}
	for _, raw := range r.Capabilities {
		if !access.Capability(raw).Known() {
			errs = append(errs, fmt.Errorf(
				"%s: rules[%d]: %q is not a capability (have %s)",
				at, j, raw, access.KnownCapabilities().String()))
		}
	}
	return errs
}

// AccessPolicyNames lists the configured policy names, sorted.
func (c *Config) AccessPolicyNames() []string {
	out := make([]string, 0, len(c.AccessPolicies))
	for _, p := range c.AccessPolicies {
		out = append(out, p.Name)
	}
	sort.Strings(out)
	return out
}
