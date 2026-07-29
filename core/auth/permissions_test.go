package auth

import (
	"testing"

	"github.com/fondaco-dev/fondaco/core/api"
)

func TestPublishers(t *testing.T) {
	tokenID := api.Identity{Kind: api.IdentityToken, Subject: "ci-bot"}
	oidcID := api.Identity{
		Kind: api.IdentityOIDC, Subject: "project_path:group/app:ref:main",
		ProjectPath: "group/app", Ref: "main",
	}
	otherProject := api.Identity{Kind: api.IdentityOIDC, Subject: "s", ProjectPath: "other/app"}

	tests := []struct {
		name     string
		patterns []string
		id       api.Identity
		want     bool
	}{
		{"empty denies", nil, tokenID, false},
		{"anonymous always denied", []string{"*"}, api.Anonymous(), false},
		{"wildcard allows authenticated", []string{"*"}, tokenID, true},
		{"exact token", []string{"token:ci-bot"}, tokenID, true},
		{"other token", []string{"token:other"}, tokenID, false},
		{"project path", []string{"project:group/app"}, oidcID, true},
		{"project glob", []string{"project:group/*"}, oidcID, true},
		{"project glob no cross slash", []string{"project:group/*"}, otherProject, false},
		{"project with ref", []string{"project:group/app@main"}, oidcID, true},
		{"project with other ref", []string{"project:group/app@release"}, oidcID, false},
		{"token pattern does not match oidc", []string{"token:*"}, oidcID, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := NewPublishers(tt.patterns)
			if err != nil {
				t.Fatal(err)
			}
			if got := p.Allowed(tt.id); got != tt.want {
				t.Errorf("Allowed(%+v) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}

	if _, err := NewPublishers([]string{""}); err == nil {
		t.Error("empty pattern accepted")
	}
	if _, err := NewPublishers([]string{"[bad"}); err == nil {
		t.Error("malformed glob accepted")
	}
}
