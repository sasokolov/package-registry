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
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sasokolov/package-registry/core/api"
	"github.com/sasokolov/package-registry/core/state"
)

// RevokeTx marks a token revoked inside the caller's transaction and
// returns its hash, so a revocation and its replication journal entry
// commit together. Revocation is idempotent and one-way: a token is never
// un-revoked (invariant 14).
func RevokeTx(ctx context.Context, tx pgx.Tx, name string) (hash string, err error) {
	err = tx.QueryRow(ctx, `
		UPDATE tokens
		   SET revoked_at = COALESCE(revoked_at, now()), updated_at = now()
		 WHERE name = $1
		RETURNING hash`, name).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("token %q: %w", name, api.ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("revoke token: %w", err)
	}
	return hash, nil
}

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
	lookup   LookupFunc
	ttl      time.Duration
	staleFor time.Duration
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]tokenCacheEntry
}

type tokenCacheEntry struct {
	name    string
	expires time.Time
	// staleUntil bounds how long this verdict may still be served after
	// the TTL when the backend is unreachable. Revocations still take
	// effect: the sweeper evicts revoked hashes, and a database that is
	// down cannot have accepted a revocation either.
	staleUntil time.Time
}

// NewTokenVerifier builds a verifier; ttl bounds the cache.
func NewTokenVerifier(lookup LookupFunc, ttl time.Duration) *TokenVerifier {
	return &TokenVerifier{
		lookup:   lookup,
		ttl:      ttl,
		staleFor: defaultStaleWindow,
		now:      time.Now,
		cache:    make(map[string]tokenCacheEntry),
	}
}

// SetStaleWindow bounds how long an already-verified identity may be reused
// while the backend is unreachable. A non-positive window disables the
// fallback: an outage past the cache TTL then degrades loudly.
func (v *TokenVerifier) SetStaleWindow(d time.Duration) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.staleFor = d
}

// staleWindow reads the configured window.
func (v *TokenVerifier) staleWindow() time.Duration {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.staleFor
}

// defaultStaleWindow is how long a verified identity may be reused while
// the token backend is unreachable (invariant 7: a database outage degrades
// the read path, it does not stop it).
const defaultStaleWindow = 6 * time.Hour

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
		now := v.now()
		v.mu.Lock()
		v.cache[hash] = tokenCacheEntry{
			name:       name,
			expires:    now.Add(v.ttl),
			staleUntil: now.Add(v.staleFor),
		}
		v.mu.Unlock()
		return api.Identity{Kind: api.IdentityToken, Subject: name}, nil
	case errors.Is(err, api.ErrNotFound):
		v.mu.Lock()
		delete(v.cache, hash)
		v.mu.Unlock()
		return api.Identity{}, fmt.Errorf("unknown token (hash %s…): %w", hash[:8], api.ErrUnauthorized)
	default:
		// An UNAVAILABLE backend is the only case that degrades: a
		// malformed row or a decode error is a real failure and must not
		// silently authenticate anyone. Within the stale window a
		// previously verified identity keeps working rather than turning a
		// database outage into an authentication outage (invariant 7);
		// revocation is unaffected, since the sweeper evicts revoked
		// hashes and a database that is down cannot accept new ones.
		if !errors.Is(err, api.ErrUnavailable) && !isBackendUnreachable(err) {
			return api.Identity{}, fmt.Errorf("token lookup failed (hash %s…): %w", hash[:8], err)
		}
		if ok && v.staleWindow() > 0 && v.now().Before(e.staleUntil) {
			return api.Identity{Kind: api.IdentityToken, Subject: e.name, Stale: true}, nil
		}
		return api.Identity{}, fmt.Errorf("token backend unavailable (hash %s…): %w", hash[:8], api.ErrUnavailable)
	}
}

// Revoke evicts hashes from the cache. Revocation must take effect within
// the sweeper interval, not the cache TTL: a token revoked here or at
// another geo site has to stop working promptly (invariant 14).
func (v *TokenVerifier) Revoke(hashes []string) int {
	if len(hashes) == 0 {
		return 0
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	var evicted int
	for _, h := range hashes {
		if _, ok := v.cache[h]; ok {
			delete(v.cache, h)
			evicted++
		}
	}
	return evicted
}

// RevocationSource lists tokens revoked recently enough that a cached
// identity could still exist.
type RevocationSource interface {
	RecentlyRevoked(ctx context.Context, since time.Duration) ([]string, error)
}

// WatchRevocations evicts revoked tokens from the cache on an interval. It
// covers both local revocations (a CLI in another process) and replicated
// ones (the applier writes revoked_at). A database outage only delays
// eviction; it never breaks the read path.
func (v *TokenVerifier) WatchRevocations(ctx context.Context, src RevocationSource, interval time.Duration, logger *slog.Logger) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	// Look back further than the cache TTL so nothing cached can be missed
	// after a restart or a slow tick.
	lookback := v.ttl * 4
	if lookback < time.Minute {
		lookback = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		hashes, err := src.RecentlyRevoked(ctx, lookback)
		if err != nil {
			logger.Debug("revocation sweep failed", "error", err)
			continue
		}
		if n := v.Revoke(hashes); n > 0 {
			logger.Info("revoked tokens evicted from the auth cache", "count", n)
		}
	}
}

// isBackendUnreachable reports whether an error means "the token backend is
// not answering" as opposed to "the backend answered with a problem".
func isBackendUnreachable(err error) bool {
	if errors.Is(err, state.ErrUnavailable) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// Tokens is the PostgreSQL-backed token store.
type Tokens struct {
	db *state.DB
}

// NewTokens wraps the state DB.
func NewTokens(db *state.DB) *Tokens { return &Tokens{db: db} }

// RecentlyRevoked implements RevocationSource.
func (t *Tokens) RecentlyRevoked(ctx context.Context, since time.Duration) ([]string, error) {
	rows, err := t.db.Pool().Query(ctx,
		"SELECT hash FROM tokens WHERE revoked_at IS NOT NULL AND revoked_at > now() - $1::interval",
		fmt.Sprintf("%d seconds", int(since.Seconds())))
	if err != nil {
		return nil, fmt.Errorf("list revoked tokens: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

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
