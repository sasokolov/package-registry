package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sasokolov/package-registry/core/api"
)

// fakeLookup is a scriptable LookupFunc backend.
type fakeLookup struct {
	tokens map[string]string // hash -> name
	down   bool
	calls  int
}

func (f *fakeLookup) lookup(_ context.Context, hash string) (string, error) {
	f.calls++
	if f.down {
		return "", errors.New("connection refused")
	}
	name, ok := f.tokens[hash]
	if !ok {
		return "", api.ErrNotFound
	}
	return name, nil
}

func TestTokenVerifier(t *testing.T) {
	secret := TokenPrefix + "unit-test-secret"
	backend := &fakeLookup{tokens: map[string]string{hashHex(secret): "ci-bot"}}
	v := NewTokenVerifier(backend.lookup, 5*time.Minute)
	now := time.Unix(1000, 0)
	v.now = func() time.Time { return now }
	ctx := t.Context()

	id, err := v.Authenticate(ctx, secret)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.Kind != api.IdentityToken || id.Subject != "ci-bot" {
		t.Errorf("identity = %+v", id)
	}
	if backend.calls != 1 {
		t.Fatalf("backend calls = %d, want 1", backend.calls)
	}

	// Within TTL: served from cache, no backend hit.
	if _, err := v.Authenticate(ctx, secret); err != nil {
		t.Fatal(err)
	}
	if backend.calls != 1 {
		t.Errorf("backend calls after cached hit = %d, want 1", backend.calls)
	}

	// Backend down + fresh cache: reads keep working (invariant 7).
	backend.down = true
	if _, err := v.Authenticate(ctx, secret); err != nil {
		t.Errorf("Authenticate with down backend and fresh cache: %v", err)
	}

	// TTL expired + backend down: loud degradation.
	now = now.Add(6 * time.Minute)
	if _, err := v.Authenticate(ctx, secret); !errors.Is(err, api.ErrUnavailable) {
		t.Errorf("Authenticate = %v, want ErrUnavailable", err)
	}

	// Backend up again: cache repopulates.
	backend.down = false
	if _, err := v.Authenticate(ctx, secret); err != nil {
		t.Errorf("Authenticate after recovery: %v", err)
	}
}

func TestTokenVerifierUnknownToken(t *testing.T) {
	backend := &fakeLookup{tokens: map[string]string{}}
	v := NewTokenVerifier(backend.lookup, time.Minute)

	_, err := v.Authenticate(t.Context(), TokenPrefix+"nope")
	if !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("Authenticate = %v, want ErrUnauthorized", err)
	}
	// The error may carry only a hash prefix, never the secret itself.
	if strings.Contains(err.Error(), "nope") {
		t.Errorf("error leaks the secret: %q", err)
	}
}

func TestHashPrefixNeverLeaksSecret(t *testing.T) {
	secret := TokenPrefix + "super-secret-value"
	p := HashPrefix(secret)
	if len(p) != 8 {
		t.Errorf("HashPrefix length = %d, want 8", len(p))
	}
	if strings.Contains(secret, p) {
		t.Error("hash prefix appears inside the secret — suspicious")
	}
}
