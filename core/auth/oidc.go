package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"

	"github.com/sasokolov/package-registry/core/api"
	"github.com/sasokolov/package-registry/core/config"
)

// clockSkew tolerated when validating token time claims.
const clockSkew = 60 * time.Second

// OIDC validates id_tokens from the configured trusted issuers and maps
// their claims onto api.Identity (sub, project_path, ref).
type OIDC struct {
	client  *http.Client
	cache   *jwk.Cache
	issuers map[string]*issuerVerifier // keyed by issuer URL
}

type issuerVerifier struct {
	cfg config.OIDCIssuer

	mu      sync.Mutex
	jwksURL string  // resolved lazily via OIDC discovery when not configured
	set     jwk.Set // cached set bound to jwksURL
}

// NewOIDC builds the validator. JWKS endpoints are resolved and fetched
// lazily on first use, so construction works while issuers are unreachable.
func NewOIDC(ctx context.Context, issuers []config.OIDCIssuer, client *http.Client) *OIDC {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	o := &OIDC{
		client:  client,
		cache:   jwk.NewCache(ctx),
		issuers: make(map[string]*issuerVerifier, len(issuers)),
	}
	for _, iss := range issuers {
		o.issuers[iss.Issuer] = &issuerVerifier{cfg: iss}
	}
	return o
}

// Enabled reports whether any issuer is configured.
func (o *OIDC) Enabled() bool { return len(o.issuers) > 0 }

// Authenticate validates a raw compact JWT and maps it to an identity.
func (o *OIDC) Authenticate(ctx context.Context, raw string) (api.Identity, error) {
	// Peek at the issuer without verification to pick the right key set;
	// the real parse below verifies signature and claims.
	peeked, err := jwt.ParseInsecure([]byte(raw))
	if err != nil {
		return api.Identity{}, fmt.Errorf("malformed JWT: %w", api.ErrUnauthorized)
	}
	v, ok := o.issuers[peeked.Issuer()]
	if !ok {
		return api.Identity{}, fmt.Errorf("issuer %q is not trusted: %w", peeked.Issuer(), api.ErrUnauthorized)
	}

	set, err := o.keySet(ctx, v)
	if err != nil {
		return api.Identity{}, fmt.Errorf("JWKS for %s unavailable: %w", v.cfg.Issuer, err)
	}

	tok, err := jwt.Parse([]byte(raw),
		jwt.WithKeySet(set),
		jwt.WithValidate(true),
		jwt.WithIssuer(v.cfg.Issuer),
		jwt.WithAudience(v.cfg.Audience),
		jwt.WithAcceptableSkew(clockSkew),
	)
	if err != nil {
		return api.Identity{}, fmt.Errorf("JWT rejected: %v: %w", err, api.ErrUnauthorized)
	}

	id := api.Identity{Kind: api.IdentityOIDC, Subject: tok.Subject(), Issuer: v.cfg.Issuer}
	if s, ok := stringClaim(tok, "project_path"); ok {
		id.ProjectPath = s
	}
	if s, ok := stringClaim(tok, "ref"); ok {
		id.Ref = s
	}
	return id, nil
}

func stringClaim(tok jwt.Token, name string) (string, bool) {
	v, ok := tok.Get(name)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// keySet returns the (auto-refreshing) JWKS for an issuer, resolving the
// endpoint via OIDC discovery on first use.
func (o *OIDC) keySet(ctx context.Context, v *issuerVerifier) (jwk.Set, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.set != nil {
		return v.set, nil
	}

	jwksURL := v.cfg.JWKSURL
	if jwksURL == "" {
		discovered, err := o.discoverJWKS(ctx, v.cfg.Issuer)
		if err != nil {
			return nil, err
		}
		jwksURL = discovered
	}
	if err := o.cache.Register(jwksURL,
		jwk.WithHTTPClient(o.client),
		jwk.WithMinRefreshInterval(15*time.Minute),
	); err != nil {
		return nil, fmt.Errorf("register JWKS %s: %w", jwksURL, err)
	}
	// Prime the cache so a broken endpoint is reported now, not on parse.
	if _, err := o.cache.Refresh(ctx, jwksURL); err != nil {
		return nil, fmt.Errorf("fetch JWKS %s: %w", jwksURL, err)
	}
	v.jwksURL = jwksURL
	v.set = jwk.NewCachedSet(o.cache, jwksURL)
	return v.set, nil
}

// discoverJWKS resolves jwks_uri from the issuer's OIDC discovery document.
func (o *OIDC) discoverJWKS(ctx context.Context, issuer string) (string, error) {
	wellKnown := issuer + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wellKnown, nil)
	if err != nil {
		return "", fmt.Errorf("discovery request: %w", err)
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("OIDC discovery %s: %w", wellKnown, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("OIDC discovery %s: status %d", wellKnown, resp.StatusCode)
	}
	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", fmt.Errorf("OIDC discovery %s: decode: %w", wellKnown, err)
	}
	if doc.JWKSURI == "" {
		return "", fmt.Errorf("OIDC discovery %s: no jwks_uri", wellKnown)
	}
	return doc.JWKSURI, nil
}
