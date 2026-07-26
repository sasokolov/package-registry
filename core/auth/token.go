// Package auth resolves request credentials into api.Identity: static
// tokens (sha256 hash in PostgreSQL, in-memory TTL cache) and OIDC id_tokens
// validated against issuer JWKS (GitLab CI).
//
// Secrets are never logged; log lines and errors carry at most the first
// 8 characters of the token hash (invariant 12).
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sasokolov/package-registry/core/api"
	"github.com/sasokolov/package-registry/core/state"
)

// TokenPrefix marks static registry tokens in the Authorization header.
const TokenPrefix = "reg_"

// hashHex returns the hex sha256 of a secret; only its first 8 chars may
// appear in logs.
func hashHex(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

// HashPrefix is the loggable prefix of a token's hash.
func HashPrefix(secret string) string { return hashHex(secret)[:8] }

// LookupFunc resolves a token hash to the token name. It returns
// api.ErrNotFound for unknown or revoked tokens and any other error when the
// backend is unavailable.
type LookupFunc func(ctx context.Context, hashHex string) (name string, err error)

// TokenVerifier authenticates static tokens with an in-memory TTL cache in
// front of the lookup backend, so cached identities keep working while
// PostgreSQL is down (invariant 7).
type TokenVerifier struct {
	lookup LookupFunc
	ttl    time.Duration
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]tokenCacheEntry
}

type tokenCacheEntry struct {
	name    string
	expires time.Time
}

// NewTokenVerifier builds a verifier; ttl bounds the cache.
func NewTokenVerifier(lookup LookupFunc, ttl time.Duration) *TokenVerifier {
	return &TokenVerifier{
		lookup: lookup,
		ttl:    ttl,
		now:    time.Now,
		cache:  make(map[string]tokenCacheEntry),
	}
}

// Authenticate resolves a presented secret to an identity.
func (v *TokenVerifier) Authenticate(ctx context.Context, secret string) (api.Identity, error) {
	hash := hashHex(secret)

	v.mu.Lock()
	e, ok := v.cache[hash]
	v.mu.Unlock()
	if ok && v.now().Before(e.expires) {
		return api.Identity{Kind: api.IdentityToken, Subject: e.name}, nil
	}

	name, err := v.lookup(ctx, hash)
	switch {
	case err == nil:
		v.mu.Lock()
		v.cache[hash] = tokenCacheEntry{name: name, expires: v.now().Add(v.ttl)}
		v.mu.Unlock()
		return api.Identity{Kind: api.IdentityToken, Subject: name}, nil
	case errors.Is(err, api.ErrNotFound):
		v.mu.Lock()
		delete(v.cache, hash)
		v.mu.Unlock()
		return api.Identity{}, fmt.Errorf("unknown token (hash %s…): %w", hash[:8], api.ErrUnauthorized)
	default:
		// Backend down and no fresh cache entry: degrade loudly.
		return api.Identity{}, fmt.Errorf("token backend unavailable (hash %s…): %w", hash[:8], api.ErrUnavailable)
	}
}

// Tokens is the PostgreSQL-backed token store.
type Tokens struct {
	db *state.DB
}

// NewTokens wraps the state DB.
func NewTokens(db *state.DB) *Tokens { return &Tokens{db: db} }

// Lookup implements LookupFunc.
func (t *Tokens) Lookup(ctx context.Context, hash string) (string, error) {
	var name string
	err := t.db.Pool().QueryRow(ctx,
		"SELECT name FROM tokens WHERE hash = $1 AND revoked_at IS NULL", hash).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("token hash %s…: %w", hash[:8], api.ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("token lookup: %w", err)
	}
	return name, nil
}

// Create generates a fresh secret for name, stores only its hash and returns
// the secret — the sole moment it exists in cleartext.
func (t *Tokens) Create(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", errors.New("token name is required")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	secret := TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	_, err := t.db.Pool().Exec(ctx,
		"INSERT INTO tokens (name, hash) VALUES ($1, $2)", name, hashHex(secret))
	if err != nil {
		return "", fmt.Errorf("store token %q: %w", name, err)
	}
	return secret, nil
}
