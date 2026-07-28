// Package access decides who may do what, in the shape Vault made familiar:
// named policies of path rules, explicit capabilities, an explicit deny that
// wins, and a default of refusing.
//
// The paths are objects of this registry rather than URLs. A URL namespace
// would drag every format's layout into the access rules — the core would
// have to know that Maven spells a coordinate one way and NuGet another
// (invariant 1) — and an operator would have to write those layouts by hand.
// Two namespaces exist:
//
//	feed/<feed>/<coordinate>   what the registry serves
//	sys/<area>                 how the registry is run
//
// Because the unit is a coordinate and not a request path, one rule reads
// the same whether the request arrives at a feed or at a group over it, and
// "may publish com.example but not com.acme" is expressible at all.
package access

import (
	"fmt"
	"sort"
	"strings"
)

// Capability is one thing an identity may do.
type Capability string

// The capabilities. They are deliberately few: every one of them answers a
// question an operator actually asks, and a capability nobody can explain is
// a capability nobody can review.
const (
	// CapRead downloads a coordinate.
	CapRead Capability = "read"
	// CapList enumerates what exists without downloading it.
	CapList Capability = "list"
	// CapPublish writes a package.
	CapPublish Capability = "publish"
	// CapCreate makes something new that is not a package: a token.
	CapCreate Capability = "create"
	// CapUpdate changes something mutable: the configuration, a quarantine
	// decision, a conflict resolution.
	CapUpdate Capability = "update"
	// CapDelete removes something: revoking a token, dropping a feed from
	// the configuration.
	CapDelete Capability = "delete"
	// CapDeny refuses, and beats every other capability on the same path.
	CapDeny Capability = "deny"
)

// Capabilities is a set, kept ordered so error messages and API responses
// are stable.
type Capabilities []Capability

// Known reports whether c is a capability this registry understands.
func (c Capability) Known() bool {
	switch c {
	case CapRead, CapList, CapPublish, CapCreate, CapUpdate, CapDelete, CapDeny:
		return true
	default:
		return false
	}
}

// KnownCapabilities lists every capability, for validation messages and the
// API.
func KnownCapabilities() Capabilities {
	return Capabilities{CapRead, CapList, CapPublish, CapCreate, CapUpdate, CapDelete, CapDeny}
}

// Rule grants capabilities on a path.
type Rule struct {
	// Path is an exact path, or a pattern: a trailing "*" matches any
	// remainder, and "+" matches exactly one segment. Both are Vault's, and
	// they are the whole pattern language on purpose — a language rich
	// enough to be clever with is a language nobody can review.
	Path string
	// Capabilities granted (or CapDeny to refuse).
	Capabilities Capabilities
	// policy is the name of the policy this rule came from; it is what an
	// explanation names.
	policy string
}

// Policy is a named set of rules.
type Policy struct {
	Name  string
	Rules []Rule
}

// Match selects identities a binding applies to. An empty field matches
// anything, so a binding with no conditions applies to everyone — including
// anonymous callers, which is exactly how a public feed is expressed.
type Match struct {
	// Kind is "token", "oidc" or "anonymous".
	Kind string
	// Issuer is the OIDC issuer.
	Issuer string
	// Subject is the token name or the OIDC subject claim.
	Subject string
	// ProjectPath and Ref are GitLab CI claims.
	ProjectPath string
	Ref         string
	// Authenticated, when true, requires any non-anonymous identity. It is
	// what "this feed needs a login, but any login" means.
	Authenticated bool
}

// Binding attaches policies to the identities a Match selects.
type Binding struct {
	// Name identifies the binding, so an explanation can say which one
	// brought a policy into play.
	Name     string
	Policies []string
	Match    Match
}

// Identity is what the engine matches a binding against. It mirrors
// api.Identity without importing it, so this package stays a leaf.
type Identity struct {
	Kind        string
	Subject     string
	Issuer      string
	ProjectPath string
	Ref         string
}

// IsAnonymous reports whether the identity is unauthenticated.
func (id Identity) IsAnonymous() bool { return id.Kind == "anonymous" || id.Kind == "" }

// String renders the identity the way audit lines and errors name it.
func (id Identity) String() string { return id.Kind + ":" + id.Subject }

// Engine answers access questions from a compiled set of policies and
// bindings. It is immutable: a configuration reload builds a new one.
type Engine struct {
	policies map[string]Policy
	bindings []Binding
}

// New compiles policies and bindings into an engine.
func New(policies []Policy, bindings []Binding) (*Engine, error) {
	byName := make(map[string]Policy, len(policies))
	for _, p := range policies {
		if p.Name == "" {
			return nil, fmt.Errorf("a policy has no name")
		}
		if _, taken := byName[p.Name]; taken {
			return nil, fmt.Errorf("policy %q is defined twice", p.Name)
		}
		for i := range p.Rules {
			p.Rules[i].policy = p.Name
			if err := validRule(p.Name, i, p.Rules[i]); err != nil {
				return nil, err
			}
		}
		byName[p.Name] = p
	}
	for i, b := range bindings {
		if len(b.Policies) == 0 {
			return nil, fmt.Errorf("bindings[%d] attaches no policies", i)
		}
		for _, name := range b.Policies {
			if _, ok := byName[name]; !ok {
				return nil, fmt.Errorf("bindings[%d] names policy %q, which does not exist", i, name)
			}
		}
	}
	return &Engine{policies: byName, bindings: bindings}, nil
}

func validRule(policy string, i int, r Rule) error {
	if r.Path == "" {
		return fmt.Errorf("policy %s: rules[%d] has no path", policy, i)
	}
	if strings.Contains(strings.TrimSuffix(r.Path, "*"), "*") {
		return fmt.Errorf(
			"policy %s: rules[%d]: %q — \"*\" is only allowed at the end; use \"+\" for one segment",
			policy, i, r.Path)
	}
	if len(r.Capabilities) == 0 {
		return fmt.Errorf("policy %s: rules[%d] grants nothing", policy, i)
	}
	for _, c := range r.Capabilities {
		if !c.Known() {
			return fmt.Errorf("policy %s: rules[%d]: %q is not a capability (have %s)",
				policy, i, c, KnownCapabilities().String())
		}
	}
	return nil
}

// String renders capabilities as a stable, readable list.
func (c Capabilities) String() string {
	out := make([]string, 0, len(c))
	for _, capability := range c {
		out = append(out, string(capability))
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// Has reports whether the set contains a capability.
func (c Capabilities) Has(want Capability) bool {
	for _, capability := range c {
		if capability == want {
			return true
		}
	}
	return false
}

// Decision is the answer, with the reasoning attached. The reasoning is not
// decoration: an access system whose refusals cannot be explained is one
// people route around instead of fixing.
type Decision struct {
	Allowed bool
	// Policies that were attached to this identity, in binding order.
	Policies []string
	// Bindings that attached them, in the order they matched. When this is
	// empty and nothing was allowed, the mistake is in a binding's match and
	// not in any policy — which is the first thing worth knowing.
	Bindings []string
	// Rule is the path of the rule that decided, empty when nothing matched.
	Rule string
	// Policy names the policy that rule came from.
	Policy string
	// Capabilities effective at the deciding path.
	Capabilities Capabilities
	// Reason explains the outcome in one sentence.
	Reason string
}

// PoliciesFor lists the policies bound to an identity, in binding order and
// without repeats.
func (e *Engine) PoliciesFor(id Identity) []string {
	policies, _ := e.boundVia(id)
	return policies
}

// boundVia lists the policies bound to an identity and the bindings that
// bound them. The bindings are worth carrying: "no policy is bound to you"
// and "the binding you expected did not match" look identical from the
// policy list alone, and they are fixed in different places.
func (e *Engine) boundVia(id Identity) (policies, bindings []string) {
	seen := map[string]bool{}
	for _, b := range e.bindings {
		if !b.Match.matches(id) {
			continue
		}
		if b.Name != "" {
			bindings = append(bindings, b.Name)
		}
		for _, name := range b.Policies {
			if seen[name] {
				continue
			}
			seen[name] = true
			policies = append(policies, name)
		}
	}
	return policies, bindings
}

// Allowed reports whether id may exercise want on path.
func (e *Engine) Allowed(id Identity, path string, want Capability) bool {
	return e.Explain(id, path, want).Allowed
}

// Explain answers and says why.
//
// The rule is Vault's, and it is chosen rather than inherited: the most
// specific matching path decides, and among the rules at that specificity a
// deny beats everything. That is what makes "deny a whole feed, then grant
// one namespace inside it" work — the narrower grant is a deliberate
// exception, not an oversight. An absolute deny would forbid expressing
// exceptions at all, which is how people end up with no denies.
func (e *Engine) Explain(id Identity, path string, want Capability) Decision {
	names, via := e.boundVia(id)
	decision := Decision{Policies: names, Bindings: via}

	var best []Rule
	bestScore := specificity{}
	for _, name := range names {
		for _, rule := range e.policies[name].Rules {
			score, ok := match(rule.Path, path)
			if !ok {
				continue
			}
			switch {
			case len(best) == 0 || score.beats(bestScore):
				best, bestScore = []Rule{rule}, score
			case score == bestScore:
				best = append(best, rule)
			}
		}
	}

	if len(best) == 0 {
		decision.Reason = "no policy grants anything on " + path
		if len(names) == 0 {
			decision.Reason = "no policy is bound to " + id.String()
		}
		return decision
	}

	effective := Capabilities{}
	seen := map[Capability]bool{}
	for _, rule := range best {
		for _, capability := range rule.Capabilities {
			if !seen[capability] {
				seen[capability] = true
				effective = append(effective, capability)
			}
		}
	}
	decision.Capabilities = effective
	decision.Rule = best[0].Path
	decision.Policy = best[0].policy

	if effective.Has(CapDeny) {
		for _, rule := range best {
			if rule.Capabilities.Has(CapDeny) {
				decision.Rule, decision.Policy = rule.Path, rule.policy
				break
			}
		}
		decision.Reason = fmt.Sprintf("policy %s denies %s", decision.Policy, decision.Rule)
		return decision
	}

	if effective.Has(want) {
		for _, rule := range best {
			if rule.Capabilities.Has(want) {
				decision.Rule, decision.Policy = rule.Path, rule.policy
				break
			}
		}
		decision.Allowed = true
		decision.Reason = fmt.Sprintf("policy %s grants %s on %s", decision.Policy, want, decision.Rule)
		return decision
	}

	decision.Reason = fmt.Sprintf("policy %s grants %s on %s, which does not include %s",
		decision.Policy, effective.String(), decision.Rule, want)
	return decision
}

// MayReach reports whether anything under prefix could grant want to id.
//
// It is deliberately optimistic, and it is not the decision. A publish
// arrives before its coordinate is known — the module has to parse the
// upload first — so this answers the only question available at that
// moment: "is there any point reading this body at all". The real check
// happens per coordinate, where a narrow grant or a narrow deny can apply.
// Being optimistic here cannot widen access; it can only decline to refuse
// early.
func (e *Engine) MayReach(id Identity, prefix string, want Capability) bool {
	for _, name := range e.PoliciesFor(id) {
		for _, rule := range e.policies[name].Rules {
			if !rule.Capabilities.Has(want) {
				continue
			}
			literal := strings.TrimSuffix(rule.Path, "*")
			if strings.HasPrefix(literal, prefix) || strings.HasPrefix(prefix, literal) {
				return true
			}
		}
	}
	return false
}

// Policies lists every compiled policy, sorted by name.
func (e *Engine) Policies() []Policy {
	out := make([]Policy, 0, len(e.policies))
	for _, p := range e.policies {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Bindings lists the bindings in configuration order.
func (e *Engine) Bindings() []Binding { return e.bindings }

// matches reports whether an identity satisfies the conditions. Every
// condition is a glob; an empty condition is not a condition.
func (m Match) matches(id Identity) bool {
	if m.Authenticated && id.IsAnonymous() {
		return false
	}
	pairs := [][2]string{
		{m.Kind, id.Kind},
		{m.Issuer, id.Issuer},
		{m.Subject, id.Subject},
		{m.ProjectPath, id.ProjectPath},
		{m.Ref, id.Ref},
	}
	for _, pair := range pairs {
		if pair[0] == "" {
			continue
		}
		if !glob(pair[0], pair[1]) {
			return false
		}
	}
	return true
}

// glob matches a value against a pattern with a trailing "*" or exactly.
func glob(pattern, value string) bool {
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(value, prefix)
	}
	return pattern == value
}
