package config

import (
	"fmt"
	"strings"
)

// Which ways of signing in a site offers, and how they are described.
//
// The console has to render a login form, and the honest form depends on the
// site: a registry with no database issues no static tokens, one with no
// trusted issuer accepts no id_tokens, and an operator may want to offer
// only one of the two even where both would work. So the site says what it
// offers rather than the console guessing, and the answer is configuration
// like everything else.

// Auth method types.
const (
	AuthMethodToken = "token"
	AuthMethodOIDC  = "oidc"
)

// How a credential is obtained.
const (
	// FlowToken means the person already has a credential and pastes it.
	FlowToken = "token"
	// FlowBrowser means the console sends them to the issuer and gets one
	// back. It needs a client_id on the issuer: without one this registry is
	// not a registered OAuth client anywhere and has nothing to redirect as.
	FlowBrowser = "browser"
)

// AuthMethodConfig describes one way to sign in, as an operator writes it.
type AuthMethodConfig struct {
	// Type is "token" or "oidc".
	Type string `yaml:"type" json:"type"`
	// Label is what the form calls it. Empty takes a sensible default, and
	// setting it is how a site says "GitLab CI" instead of "OIDC".
	Label string `yaml:"label,omitempty" json:"label,omitempty"`
	// Issuer narrows an oidc method to one trusted issuer, which is what
	// makes several of them distinguishable on the form.
	Issuer string `yaml:"issuer,omitempty" json:"issuer,omitempty"`
	// Help is a sentence shown under the field.
	Help string `yaml:"help,omitempty" json:"help,omitempty"`
	// Hidden keeps a method out of the form without removing the ability to
	// use it. A credential that still authenticates but is not advertised is
	// a real state — a method being retired, or one only automation uses —
	// and pretending otherwise would mean deleting configuration to change a
	// screen.
	Hidden bool `yaml:"hidden,omitempty" json:"hidden,omitempty"`
}

// AuthMethod is one way to sign in, as the form should present it.
//
// It is derived rather than written: whether an issuer can be signed in to
// through a browser is a fact about that issuer's configuration, not a field
// somebody sets on the form and hopes matches.
type AuthMethod struct {
	Type   string `json:"type"`
	Label  string `json:"label,omitempty"`
	Issuer string `json:"issuer,omitempty"`
	Help   string `json:"help,omitempty"`
	Flow   string `json:"flow"`
}

// AuthMethods is what the login form should offer, in order.
//
// With nothing configured the list is derived from what the site can
// actually accept: static tokens need a database to verify against, and an
// id_token needs an issuer to trust. Deriving beats defaulting to both,
// because a form offering a method the site cannot honour is a form that
// wastes somebody's afternoon.
func (c *Config) AuthMethods() []AuthMethod {
	if len(c.Auth.Methods) > 0 {
		out := make([]AuthMethod, 0, len(c.Auth.Methods))
		for _, m := range c.Auth.Methods {
			if m.Hidden {
				continue
			}
			// A method that names no issuer is unambiguous when there is
			// only one; naming it here means everything downstream — the
			// label, the sign-in button — has an issuer to work with.
			if m.Type == AuthMethodOIDC && m.Issuer == "" && len(c.Auth.OIDC) == 1 {
				m.Issuer = c.Auth.OIDC[0].Issuer
			}
			out = append(out, c.present(m))
		}
		return out
	}

	var derived []AuthMethod
	if c.Database.DSN != "" {
		derived = append(derived, c.present(AuthMethodConfig{Type: AuthMethodToken}))
	}
	for _, issuer := range c.Auth.OIDC {
		derived = append(derived,
			c.present(AuthMethodConfig{Type: AuthMethodOIDC, Issuer: issuer.Issuer}))
	}
	return derived
}

// present fills in the wording a site did not bother to write, and the flow
// it does not get to choose.
func (c *Config) present(m AuthMethodConfig) AuthMethod {
	out := AuthMethod{
		Type:   m.Type,
		Label:  m.Label,
		Issuer: m.Issuer,
		Help:   m.Help,
		Flow:   FlowToken,
	}

	switch m.Type {
	case AuthMethodToken:
		if out.Label == "" {
			out.Label = "Registry token"
		}
		if out.Help == "" {
			out.Help = "The token your CI uses. Paste it as-is."
		}
	case AuthMethodOIDC:
		if c.issuerSignsInBrowsers(m.Issuer) {
			out.Flow = FlowBrowser
		}
		name := "your identity provider"
		if m.Issuer != "" {
			name = shortIssuer(m.Issuer)
		}
		if out.Label == "" {
			switch {
			case out.Flow == FlowBrowser:
				out.Label = "Sign in with " + name
			case m.Issuer != "":
				out.Label = "id_token from " + name
			default:
				out.Label = "OIDC id_token"
			}
		}
		if out.Help == "" {
			if out.Flow == FlowBrowser {
				out.Help = "Opens " + name + " and comes back signed in. " +
					"A pipeline still presents its own id_token."
			} else {
				out.Help = "A JWT from a trusted issuer — the same one a pipeline presents. " +
					"Paste it as-is."
			}
		}
	}
	return out
}

// issuerSignsInBrowsers reports whether a person can be redirected to this
// issuer, rather than having to bring a token from somewhere else.
func (c *Config) issuerSignsInBrowsers(issuer string) bool {
	for _, candidate := range c.Auth.OIDC {
		if candidate.Issuer == issuer {
			return candidate.BrowserSignIn()
		}
	}
	return false
}

// shortIssuer renders an issuer URL the way a person refers to it.
func shortIssuer(issuer string) string {
	trimmed := strings.TrimPrefix(strings.TrimPrefix(issuer, "https://"), "http://")
	return strings.TrimSuffix(trimmed, "/")
}

// validateAuthMethods checks the login form's configuration.
func (c *Config) validateAuthMethods() []error {
	var errs []error
	issuers := map[string]bool{}
	for _, issuer := range c.Auth.OIDC {
		issuers[issuer.Issuer] = true
	}

	for i, m := range c.Auth.Methods {
		at := fmt.Sprintf("auth.methods[%d]", i)
		switch m.Type {
		case AuthMethodToken:
			if m.Issuer != "" {
				errs = append(errs, fmt.Errorf("%s: a token method has no issuer", at))
			}
		case AuthMethodOIDC:
			if m.Issuer != "" && !issuers[m.Issuer] {
				errs = append(errs, fmt.Errorf(
					"%s: issuer %q is not among auth.oidc_issuers, so nothing it signs would be accepted",
					at, m.Issuer))
			}
			if m.Issuer == "" && len(c.Auth.OIDC) == 0 {
				errs = append(errs, fmt.Errorf(
					"%s: an oidc method needs at least one trusted issuer in auth.oidc_issuers", at))
			}
		case "":
			errs = append(errs, fmt.Errorf("%s: type is required (want %q or %q)",
				at, AuthMethodToken, AuthMethodOIDC))
		default:
			errs = append(errs, fmt.Errorf("%s: type %q is not supported (want %q or %q)",
				at, m.Type, AuthMethodToken, AuthMethodOIDC))
		}
	}
	return errs
}
