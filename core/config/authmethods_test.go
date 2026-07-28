package config

import (
	"strings"
	"testing"
)

// A login form that offers a method the site cannot honour costs somebody an
// afternoon of pasting a credential that was never going to work. So with
// nothing configured the list is derived from what the site can actually
// accept, not defaulted to everything.
func TestAuthMethodsAreDerivedFromWhatTheSiteCanAccept(t *testing.T) {
	tests := []struct {
		name  string
		cfg   Config
		want  []string // types, in order
		label string   // expected label of the last method, if set
	}{
		{
			name: "no database and no issuer offers nothing",
			cfg:  Config{},
			want: nil,
		},
		{
			name: "a database means static tokens",
			cfg:  Config{Database: DatabaseConfig{DSN: "postgres://x"}},
			want: []string{AuthMethodToken},
		},
		{
			name: "a trusted issuer means id_tokens, named after it",
			cfg: Config{
				Auth: AuthConfig{OIDC: []OIDCIssuer{{Issuer: "https://gitlab.example.com/"}}},
			},
			want:  []string{AuthMethodOIDC},
			label: "id_token from gitlab.example.com",
		},
		{
			name: "both, tokens first",
			cfg: Config{
				Database: DatabaseConfig{DSN: "postgres://x"},
				Auth:     AuthConfig{OIDC: []OIDCIssuer{{Issuer: "https://gitlab.example.com"}}},
			},
			want: []string{AuthMethodToken, AuthMethodOIDC},
		},
		{
			name: "one method per issuer, so they can be told apart",
			cfg: Config{Auth: AuthConfig{OIDC: []OIDCIssuer{
				{Issuer: "https://gitlab.example.com"},
				{Issuer: "https://ci.partner.example"},
			}}},
			want:  []string{AuthMethodOIDC, AuthMethodOIDC},
			label: "id_token from ci.partner.example",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.AuthMethods()
			if len(got) != len(tc.want) {
				t.Fatalf("got %d methods %v, want %v", len(got), got, tc.want)
			}
			for i, want := range tc.want {
				if got[i].Type != want {
					t.Errorf("method %d is %q, want %q", i, got[i].Type, want)
				}
				if got[i].Label == "" {
					t.Errorf("method %d has no label; the form would have nothing to call it", i)
				}
			}
			if tc.label != "" && len(got) > 0 && got[len(got)-1].Label != tc.label {
				t.Errorf("last label is %q, want %q", got[len(got)-1].Label, tc.label)
			}
		})
	}
}

// Configuring the list replaces the derivation entirely: an operator who
// writes it down means it.
func TestConfiguredMethodsWin(t *testing.T) {
	c := Config{
		Database: DatabaseConfig{DSN: "postgres://x"},
		Auth: AuthConfig{
			OIDC: []OIDCIssuer{{Issuer: "https://gitlab.example.com"}},
			Methods: []AuthMethodConfig{
				{Type: AuthMethodOIDC, Issuer: "https://gitlab.example.com", Label: "GitLab CI"},
			},
		},
	}
	got := c.AuthMethods()
	if len(got) != 1 || got[0].Label != "GitLab CI" {
		t.Fatalf("got %v, want just the configured GitLab method", got)
	}
}

// Hiding a method takes it off the form without taking it away. A credential
// that still authenticates but is no longer advertised is a real state — a
// method being retired, or one only automation uses — and expressing it must
// not require deleting the configuration that makes it work.
func TestHiddenMethodsAreNotOffered(t *testing.T) {
	c := Config{
		Database: DatabaseConfig{DSN: "postgres://x"},
		Auth: AuthConfig{
			OIDC: []OIDCIssuer{{Issuer: "https://gitlab.example.com"}},
			Methods: []AuthMethodConfig{
				{Type: AuthMethodToken, Hidden: true},
				{Type: AuthMethodOIDC, Issuer: "https://gitlab.example.com"},
			},
		},
	}
	got := c.AuthMethods()
	if len(got) != 1 || got[0].Type != AuthMethodOIDC {
		t.Fatalf("got %v, want only the oidc method", got)
	}
}

// The form is configuration, so it is validated like configuration: a method
// nobody could use is a mistake, not a screen quirk.
func TestAuthMethodValidation(t *testing.T) {
	tests := []struct {
		name    string
		methods []AuthMethodConfig
		issuers []OIDCIssuer
		want    string
	}{
		{
			name:    "an issuer nothing trusts",
			methods: []AuthMethodConfig{{Type: AuthMethodOIDC, Issuer: "https://nobody.example"}},
			issuers: []OIDCIssuer{{Issuer: "https://gitlab.example.com"}},
			want:    "not among auth.oidc_issuers",
		},
		{
			name:    "an oidc method with no issuer anywhere",
			methods: []AuthMethodConfig{{Type: AuthMethodOIDC}},
			want:    "needs at least one trusted issuer",
		},
		{
			name:    "a token method with an issuer",
			methods: []AuthMethodConfig{{Type: AuthMethodToken, Issuer: "https://gitlab.example.com"}},
			issuers: []OIDCIssuer{{Issuer: "https://gitlab.example.com"}},
			want:    "token method has no issuer",
		},
		{
			name:    "no type at all",
			methods: []AuthMethodConfig{{Label: "Sign in somehow"}},
			want:    "type is required",
		},
		{
			name:    "a type that does not exist",
			methods: []AuthMethodConfig{{Type: "saml"}},
			want:    "not supported",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{Auth: AuthConfig{OIDC: tc.issuers, Methods: tc.methods}}
			errs := c.validateAuthMethods()
			if len(errs) == 0 {
				t.Fatalf("no error; want one mentioning %q", tc.want)
			}
			if !strings.Contains(errs[0].Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", errs[0], tc.want)
			}
		})
	}

	valid := Config{
		Auth: AuthConfig{
			OIDC:    []OIDCIssuer{{Issuer: "https://gitlab.example.com"}},
			Methods: []AuthMethodConfig{{Type: AuthMethodOIDC, Issuer: "https://gitlab.example.com"}},
		},
	}
	if errs := valid.validateAuthMethods(); len(errs) != 0 {
		t.Fatalf("a valid method was refused: %v", errs)
	}
}
