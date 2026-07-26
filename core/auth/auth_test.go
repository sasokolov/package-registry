package auth

import (
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sasokolov/package-registry/core/api"
)

func testAuthenticator(t *testing.T, secret string) *Authenticator {
	t.Helper()
	backend := &fakeLookup{tokens: map[string]string{hashHex(secret): "ci-bot"}}
	return NewAuthenticator(NewTokenVerifier(backend.lookup, time.Minute), nil)
}

func TestIdentifySchemes(t *testing.T) {
	secret := TokenPrefix + "scheme-test"
	a := testAuthenticator(t, secret)
	ctx := t.Context()

	// No header: anonymous, not an error — feeds decide whether that is ok.
	r := httptest.NewRequest("GET", "/", nil)
	id, err := a.Identify(ctx, r)
	if err != nil || !id.IsAnonymous() {
		t.Fatalf("no header: id=%+v err=%v", id, err)
	}

	// Bearer.
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+secret)
	id, err = a.Identify(ctx, r)
	if err != nil || id.Subject != "ci-bot" {
		t.Fatalf("bearer: id=%+v err=%v", id, err)
	}

	// Basic with the token as the password (maven settings.xml, Gradle).
	r = httptest.NewRequest("GET", "/", nil)
	r.SetBasicAuth("any-username", secret)
	id, err = a.Identify(ctx, r)
	if err != nil || id.Subject != "ci-bot" {
		t.Fatalf("basic: id=%+v err=%v", id, err)
	}

	// Basic with a wrong password is rejected, not treated as anonymous.
	r = httptest.NewRequest("GET", "/", nil)
	r.SetBasicAuth("user", TokenPrefix+"wrong")
	if _, err := a.Identify(ctx, r); !errors.Is(err, api.ErrUnauthorized) {
		t.Errorf("bad basic password: err = %v, want ErrUnauthorized", err)
	}

	// Basic with an empty password.
	r = httptest.NewRequest("GET", "/", nil)
	r.SetBasicAuth("user", "")
	if _, err := a.Identify(ctx, r); !errors.Is(err, api.ErrUnauthorized) {
		t.Errorf("empty basic password: err = %v, want ErrUnauthorized", err)
	}

	// Unsupported scheme.
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Negotiate abc")
	if _, err := a.Identify(ctx, r); !errors.Is(err, api.ErrUnauthorized) {
		t.Errorf("negotiate: err = %v, want ErrUnauthorized", err)
	}

	// Malformed header.
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer")
	if _, err := a.Identify(ctx, r); !errors.Is(err, api.ErrUnauthorized) {
		t.Errorf("malformed: err = %v, want ErrUnauthorized", err)
	}
}
