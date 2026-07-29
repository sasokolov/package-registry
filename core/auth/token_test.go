package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fondaco-dev/fondaco/core/api"
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
		return "", fmt.Errorf("dial: %w", api.ErrUnavailable)
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

	// TTL expired + backend down: the last verdict is reused within the
	// stale window, marked stale, rather than turning a database outage
	// into an authentication outage (invariant 7).
	now = now.Add(6 * time.Minute)
	staleID, staleErr := v.Authenticate(ctx, secret)
	if staleErr != nil {
		t.Errorf("Authenticate past the TTL with the backend down: %v", staleErr)
	}
	if !staleID.Stale {
		t.Error("identity served past the TTL is not marked stale")
	}

	// Past the stale window it does degrade loudly.
	now = now.Add(defaultStaleWindow)
	if _, err := v.Authenticate(ctx, secret); !errors.Is(err, api.ErrUnavailable) {
		t.Errorf("Authenticate past the stale window = %v, want ErrUnavailable", err)
	}
	now = now.Add(-defaultStaleWindow)

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

// A database outage must degrade the read path, not stop it: an identity
// verified before the outage keeps working within the stale window
// (invariant 7), and is marked so writes can still refuse it.
func TestVerifierServesStaleIdentityWhileBackendIsDown(t *testing.T) {
	var down bool
	lookup := func(_ context.Context, _ string) (string, error) {
		if down {
			return "", fmt.Errorf("dial: %w", api.ErrUnavailable)
		}
		return "ci-bot", nil
	}
	now := time.Now()
	v := NewTokenVerifier(lookup, time.Minute)
	v.now = func() time.Time { return now }

	secret := "reg_" + strings.Repeat("a", 40)
	if _, err := v.Authenticate(context.Background(), secret); err != nil {
		t.Fatalf("first authenticate: %v", err)
	}

	down = true
	now = now.Add(2 * time.Minute) // past the cache TTL

	id, err := v.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatalf("authenticate during outage: %v", err)
	}
	if id.Subject != "ci-bot" {
		t.Errorf("subject = %q", id.Subject)
	}
	if !id.Stale {
		t.Error("identity served during an outage is not marked stale")
	}

	// Past the stale window the outage does surface.
	now = now.Add(defaultStaleWindow)
	if _, err := v.Authenticate(context.Background(), secret); !errors.Is(err, api.ErrUnavailable) {
		t.Errorf("after the stale window: %v, want ErrUnavailable", err)
	}
}

// A revoked token must not survive on the stale path.
func TestRevocationBeatsStaleCache(t *testing.T) {
	lookup := func(_ context.Context, _ string) (string, error) { return "ci-bot", nil }
	v := NewTokenVerifier(lookup, time.Minute)
	secret := "reg_" + strings.Repeat("b", 40)
	if _, err := v.Authenticate(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	if n := v.Revoke([]string{hashHex(secret)}); n != 1 {
		t.Fatalf("Revoke evicted %d entries, want 1", n)
	}
	// With the entry gone there is nothing stale to fall back to.
	v.lookup = func(_ context.Context, _ string) (string, error) {
		return "", fmt.Errorf("connect: %w", api.ErrUnavailable)
	}
	if _, err := v.Authenticate(context.Background(), secret); !errors.Is(err, api.ErrUnavailable) {
		t.Errorf("revoked token during an outage: %v, want ErrUnavailable", err)
	}
}

// Only an unreachable backend degrades: a backend that answers with a real
// problem must not authenticate anyone from cache.
func TestStaleFallbackOnlyForUnavailableBackend(t *testing.T) {
	var failure error
	lookup := func(_ context.Context, _ string) (string, error) {
		if failure != nil {
			return "", failure
		}
		return "ci-bot", nil
	}
	now := time.Now()
	v := NewTokenVerifier(lookup, time.Minute)
	v.now = func() time.Time { return now }

	secret := "reg_" + strings.Repeat("c", 40)
	if _, err := v.Authenticate(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)

	// A decode/consistency failure is not an outage.
	failure = errors.New("scan tokens: invalid byte sequence")
	if _, err := v.Authenticate(context.Background(), secret); err == nil {
		t.Error("a malformed-row error authenticated the caller from cache")
	}

	// A genuine outage does degrade.
	failure = fmt.Errorf("lookup: %w", api.ErrUnavailable)
	id, err := v.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatalf("outage should serve the cached identity: %v", err)
	}
	if !id.Stale {
		t.Error("identity served during an outage is not marked stale")
	}

	// With the window disabled, even an outage degrades loudly.
	v.SetStaleWindow(0)
	if _, err := v.Authenticate(context.Background(), secret); !errors.Is(err, api.ErrUnavailable) {
		t.Errorf("with the fallback disabled: %v, want ErrUnavailable", err)
	}
}
