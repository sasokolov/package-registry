package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"

	"github.com/fondaco-dev/fondaco/core/api"
	"github.com/fondaco-dev/fondaco/core/config"
)

// fakeIssuer is a local OIDC issuer: discovery document, JWKS and signing.
type fakeIssuer struct {
	t      *testing.T
	server *httptest.Server
	key    jwk.Key
	pubSet jwk.Set
	flow   *browserFlow
	// claimsToBe overrides the issuer the discovery document names, so a
	// test can present a document that belongs to somebody else.
	claimsToBe string
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	raw, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	key, err := jwk.FromRaw(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := key.Set(jwk.KeyIDKey, "test-key"); err != nil {
		t.Fatal(err)
	}
	if err := key.Set(jwk.AlgorithmKey, jwa.RS256); err != nil {
		t.Fatal(err)
	}
	pub, err := key.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	pubSet := jwk.NewSet()
	if err := pubSet.AddKey(pub); err != nil {
		t.Fatal(err)
	}

	fi := &fakeIssuer{t: t, key: key, pubSet: pubSet}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		issuer := fi.server.URL
		if fi.claimsToBe != "" {
			issuer = fi.claimsToBe
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 issuer,
			"jwks_uri":               fi.server.URL + "/oauth/discovery/keys",
			"authorization_endpoint": fi.server.URL + "/oauth/authorize",
			"token_endpoint":         fi.server.URL + "/oauth/token",
		})
	})
	mux.HandleFunc("/oauth/discovery/keys", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fi.pubSet)
	})
	fi.mountBrowserFlow(mux)
	fi.server = httptest.NewServer(mux)
	t.Cleanup(fi.server.Close)
	return fi
}

func (fi *fakeIssuer) sign(t *testing.T, mutate func(b *jwt.Builder)) string {
	t.Helper()
	now := time.Now()
	b := jwt.NewBuilder().
		Issuer(fi.server.URL).
		Audience([]string{"fondaco"}).
		Subject("project_path:group/app:ref_type:branch:ref:main").
		Claim("project_path", "group/app").
		Claim("ref", "main").
		IssuedAt(now).
		Expiration(now.Add(5 * time.Minute))
	if mutate != nil {
		mutate(b)
	}
	tok, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256, fi.key))
	if err != nil {
		t.Fatal(err)
	}
	return string(signed)
}

func newOIDC(t *testing.T, fi *fakeIssuer, jwksOverride string) *OIDC {
	t.Helper()
	return NewOIDC(t.Context(), []config.OIDCIssuer{{
		Issuer:   fi.server.URL,
		Audience: "fondaco",
		JWKSURL:  jwksOverride,
	}}, fi.server.Client())
}

func TestOIDCValidTokenViaDiscovery(t *testing.T) {
	fi := newFakeIssuer(t)
	o := newOIDC(t, fi, "") // JWKS resolved through the discovery document

	id, err := o.Authenticate(t.Context(), fi.sign(t, nil))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.Kind != api.IdentityOIDC {
		t.Errorf("kind = %s", id.Kind)
	}
	if id.ProjectPath != "group/app" || id.Ref != "main" {
		t.Errorf("claims mapped wrong: %+v", id)
	}
	if id.Subject == "" {
		t.Error("subject empty")
	}
}

func TestOIDCExplicitJWKSURL(t *testing.T) {
	fi := newFakeIssuer(t)
	o := newOIDC(t, fi, fi.server.URL+"/oauth/discovery/keys")
	if _, err := o.Authenticate(t.Context(), fi.sign(t, nil)); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
}

func TestOIDCRejections(t *testing.T) {
	fi := newFakeIssuer(t)
	o := newOIDC(t, fi, "")
	ctx := t.Context()

	tests := []struct {
		name string
		raw  string
	}{
		{"garbage", "not.a.jwt"},
		{"wrong audience", fi.sign(t, func(b *jwt.Builder) { b.Audience([]string{"other"}) })},
		{"expired", fi.sign(t, func(b *jwt.Builder) {
			b.IssuedAt(time.Now().Add(-2 * time.Hour)).Expiration(time.Now().Add(-1 * time.Hour))
		})},
		{"untrusted issuer", fi.sign(t, func(b *jwt.Builder) { b.Issuer("https://evil.example.com") })},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := o.Authenticate(ctx, tt.raw); !errors.Is(err, api.ErrUnauthorized) {
				t.Errorf("Authenticate = %v, want ErrUnauthorized", err)
			}
		})
	}
}

func TestOIDCWrongKeyRejected(t *testing.T) {
	fi := newFakeIssuer(t)
	other := newFakeIssuer(t) // different key pair

	// Token claims fi as issuer but is signed with other's key.
	forged := other.sign(t, func(b *jwt.Builder) { b.Issuer(fi.server.URL) })
	o := newOIDC(t, fi, "")
	if _, err := o.Authenticate(t.Context(), forged); !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("forged token accepted: %v", err)
	}
}
