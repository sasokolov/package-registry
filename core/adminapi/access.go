package adminapi

import (
	"net/http"
	"strings"

	"github.com/sasokolov/package-registry/core/access"
	"github.com/sasokolov/package-registry/core/api"
	"github.com/sasokolov/package-registry/core/config"
)

// The access surface: what the rules say, and why a particular request
// would be answered the way it would.
//
// The explain endpoint is not a convenience. An access system whose
// refusals cannot be accounted for is one that people work around — a
// second token, a wider policy, an exception that outlives its reason —
// and every one of those is a hole nobody wrote down.

// AccessPolicyView is one policy as the API reports it.
type AccessPolicyView struct {
	Name string `json:"name"`
	// Generated marks a policy compiled from a feed's anonymous/publishers
	// or the site's admins rather than written by hand.
	Generated bool             `json:"generated,omitempty"`
	Rules     []AccessRuleView `json:"rules"`
}

// AccessRuleView is one rule.
type AccessRuleView struct {
	Path         string   `json:"path"`
	Capabilities []string `json:"capabilities"`
}

// BindingView is one binding. The name is what makes it addressable — in the
// API, in a Terraform configuration, and in the sentence someone writes when
// they ask why a binding exists. The ones compiled from anonymous/publishers
// have none, and are marked instead.
type BindingView struct {
	Name      string            `json:"name,omitempty"`
	Generated bool              `json:"generated,omitempty"`
	Policies  []string          `json:"policies"`
	Match     map[string]string `json:"match,omitempty"`
}

// handleAccess reports the compiled rules — hand-written and generated
// alike, because what is in force is the union and reviewing half of it is
// worse than reviewing none.
func (s *Server) handleAccess(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, config.SysConfig, access.CapRead); !ok {
		return
	}
	engine := s.deps.Access()
	if engine == nil {
		s.writeError(w, http.StatusServiceUnavailable, "access rules are not loaded")
		return
	}

	policies := make([]AccessPolicyView, 0)
	for _, p := range engine.Policies() {
		view := AccessPolicyView{
			Name:      p.Name,
			Generated: strings.HasPrefix(p.Name, "feed:") || strings.HasPrefix(p.Name, "sys:"),
			Rules:     make([]AccessRuleView, 0, len(p.Rules)),
		}
		for _, rule := range p.Rules {
			caps := make([]string, 0, len(rule.Capabilities))
			for _, c := range rule.Capabilities {
				caps = append(caps, string(c))
			}
			view.Rules = append(view.Rules, AccessRuleView{Path: rule.Path, Capabilities: caps})
		}
		policies = append(policies, view)
	}

	bindings := make([]BindingView, 0)
	for _, b := range engine.Bindings() {
		bindings = append(bindings, BindingView{
			Name:      b.Name,
			Generated: b.Name == "",
			Policies:  b.Policies,
			Match:     matchView(b.Match),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"policies":     policies,
		"bindings":     bindings,
		"capabilities": capabilityNames(),
	})
}

// ExplainResponse is the answer to "would this be allowed, and why".
type ExplainResponse struct {
	Path         string   `json:"path"`
	Capability   string   `json:"capability"`
	Identity     string   `json:"identity"`
	Allowed      bool     `json:"allowed"`
	Reason       string   `json:"reason"`
	Policy       string   `json:"policy,omitempty"`
	Rule         string   `json:"rule,omitempty"`
	Policies     []string `json:"policies,omitempty"`
	Bindings     []string `json:"bindings,omitempty"`
	Capabilities []string `json:"effective_capabilities,omitempty"`
}

// handleExplain answers what the rules would decide.
//
// By default it answers about the caller, which is the question a developer
// actually has ("why can't I publish this"). An administrator may ask about
// somebody else by describing an identity, which is the question an operator
// has ("what will this pipeline be able to do") — and being able to ask it
// before granting is the difference between reviewing access and guessing.
func (s *Server) handleExplain(w http.ResponseWriter, r *http.Request) {
	caller := s.identity(r)
	subject := caller

	query := r.URL.Query()
	described := api.Identity{
		Kind:        api.IdentityKind(query.Get("kind")),
		Subject:     query.Get("subject"),
		Issuer:      query.Get("issuer"),
		ProjectPath: query.Get("project_path"),
		Ref:         query.Get("ref"),
	}
	asksAboutSomeoneElse := described.Kind != "" || described.Subject != "" ||
		described.ProjectPath != "" || described.Issuer != "" || described.Ref != ""

	if asksAboutSomeoneElse {
		// Asking what somebody else may do is reading the access rules.
		if _, ok := s.require(w, r, config.SysConfig, access.CapRead); !ok {
			return
		}
		if described.Kind == "" {
			described.Kind = api.IdentityToken
		}
		subject = described
	}

	path := query.Get("path")
	if path == "" {
		s.writeError(w, http.StatusBadRequest,
			"path is required, e.g. path=feed/releases/maven:com.example:lib@1.0.0")
		return
	}
	want := access.Capability(query.Get("capability"))
	if want == "" {
		want = access.CapRead
	}
	if !want.Known() {
		s.writeError(w, http.StatusBadRequest,
			"capability "+string(want)+" is not one of "+access.KnownCapabilities().String())
		return
	}

	d := s.allows(subject, path, want)
	out := ExplainResponse{
		Path:       path,
		Capability: string(want),
		Identity:   subject.String(),
		Allowed:    d.Allowed,
		Reason:     d.Reason,
		Policy:     d.Policy,
		Rule:       d.Rule,
		Policies:   d.Policies,
		Bindings:   d.Bindings,
	}
	for _, c := range d.Capabilities {
		out.Capabilities = append(out.Capabilities, string(c))
	}
	writeJSON(w, http.StatusOK, out)
}

func matchView(m access.Match) map[string]string {
	out := map[string]string{}
	for key, value := range map[string]string{
		"kind":         m.Kind,
		"issuer":       m.Issuer,
		"subject":      m.Subject,
		"project_path": m.ProjectPath,
		"ref":          m.Ref,
	} {
		if value != "" {
			out[key] = value
		}
	}
	if m.Authenticated {
		out["authenticated"] = "true"
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func capabilityNames() []string {
	known := access.KnownCapabilities()
	out := make([]string, 0, len(known))
	for _, c := range known {
		out = append(out, string(c))
	}
	return out
}

// handleAuthMethods says how this site can be signed in to.
//
// It is anonymous on purpose: a login form has to read it before anybody has
// logged in. Nothing here is a secret — it is the list of doors, not the
// keys — and a console that had to guess would offer methods the site
// cannot honour.
func (s *Server) handleAuthMethods(w http.ResponseWriter, r *http.Request) {
	_ = r
	methods := s.manager.Current().AuthMethods()
	if methods == nil {
		methods = []config.AuthMethodConfig{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"methods": methods})
}
