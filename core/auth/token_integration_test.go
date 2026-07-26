//go:build integration

package auth

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sasokolov/package-registry/core/api"
	"github.com/sasokolov/package-registry/core/state"
)

func openDB(t *testing.T) *state.DB {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; run via make test-integration")
	}
	db, err := state.Open(t.Context(), dsn, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestTokensCreateAndAuthenticate(t *testing.T) {
	db := openDB(t)
	ctx := t.Context()
	tokens := NewTokens(db)
	name := fmt.Sprintf("it-bot-%d", time.Now().UnixNano())

	secret, err := tokens.Create(ctx, name)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(secret, TokenPrefix) {
		t.Errorf("secret %q lacks prefix", secret)
	}

	v := NewTokenVerifier(tokens.Lookup, time.Minute)
	id, err := v.Authenticate(ctx, secret)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.Subject != name || id.Kind != api.IdentityToken {
		t.Errorf("identity = %+v", id)
	}

	// Duplicate name must be rejected by the unique constraint.
	if _, err := tokens.Create(ctx, name); err == nil {
		t.Error("duplicate token name accepted")
	}

	// Revocation: token disappears for a fresh verifier (no cache).
	if _, err := db.Pool().Exec(ctx, "UPDATE tokens SET revoked_at = now() WHERE name = $1", name); err != nil {
		t.Fatal(err)
	}
	fresh := NewTokenVerifier(tokens.Lookup, time.Minute)
	if _, err := fresh.Authenticate(ctx, secret); !errors.Is(err, api.ErrUnauthorized) {
		t.Errorf("Authenticate revoked = %v, want ErrUnauthorized", err)
	}
}
