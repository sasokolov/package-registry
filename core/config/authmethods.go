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

// AuthMethodConfig describes one way to sign in, as the login form should
// present it.
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

// AuthMethods is what the login form should offer, in order.
//
// With nothing configured the list is derived from what the site can
// actually accept: static tokens need a database to verify against, and an
// id_token needs an issuer to trust. Deriving beats defaulting to both,
// because a form offering a method the site cannot honour is a form that
// wastes somebody's afternoon.
func (c *Config) AuthMethods() []AuthMethodConfig {
	if len(c.Auth.Methods) > 0 {
		out := make([]AuthMethodConfig, 0, len(c.Auth.Methods))
		for _, m := range c.Auth.Methods {
			if m.Hidden {
				continue
			}
			out = append(out, m.withDefaults())
		}
		return out
	}

	var derived []AuthMethodConfig
	if c.Database.DSN != "" {
		derived = append(derived, AuthMethodConfig{Type: AuthMethodToken}.withDefaults())
	}
	for _, issuer := range c.Auth.OIDC {
		derived = append(derived,
			AuthMethodConfig{Type: AuthMethodOIDC, Issuer: issuer.Issuer}.withDefaults())
	}
	return derived
}

// withDefaults fills in the wording a site did not bother to write.
func (m AuthMethodConfig) withDefaults() AuthMethodConfig {
	switch m.Type {
	case AuthMethodToken:
		if m.Label == "" {
			m.Label = "Registry token"
		}
		if m.Help == "" {
			m.Help = "The token your CI uses. Paste it as-is."
		}
	case AuthMethodOIDC:
		if m.Label == "" {
			m.Label = "OIDC id_token"
			if m.Issuer != "" {
				m.Label = "id_token from " + shortIssuer(m.Issuer)
			}
		}
		if m.Help == "" {
			m.Help = "A JWT from a trusted issuer — the same one a pipeline presents. " +
				"There is no redirect sign-in: paste the token."
		}
	}
	return m
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
