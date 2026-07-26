package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/sasokolov/package-registry/core/api"
)

// Authenticator resolves an HTTP request into an identity.
type Authenticator struct {
	tokens *TokenVerifier // nil when the registry runs without a database
	oidc   *OIDC          // nil or empty when no issuers are configured
}

// NewAuthenticator combines the credential backends; either may be nil.
func NewAuthenticator(tokens *TokenVerifier, oidc *OIDC) *Authenticator {
	return &Authenticator{tokens: tokens, oidc: oidc}
}

// Identify maps the request's Authorization header to an identity:
//
//	absent header          -> anonymous identity, nil error
//	Bearer reg_...         -> static token
//	Bearer <compact JWT>   -> OIDC id_token
//	anything else          -> ErrUnauthorized
//
// Whether an anonymous identity may read a feed is the server's decision.
func (a *Authenticator) Identify(ctx context.Context, r *http.Request) (api.Identity, error) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return api.Anonymous(), nil
	}
	scheme, cred, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || cred == "" {
		return api.Identity{}, fmt.Errorf("unsupported Authorization scheme: %w", api.ErrUnauthorized)
	}
	cred = strings.TrimSpace(cred)

	switch {
	case strings.HasPrefix(cred, TokenPrefix):
		if a.tokens == nil {
			return api.Identity{}, fmt.Errorf("static tokens are not enabled (no database): %w", api.ErrUnauthorized)
		}
		return a.tokens.Authenticate(ctx, cred)
	case strings.Count(cred, ".") == 2:
		if a.oidc == nil || !a.oidc.Enabled() {
			return api.Identity{}, fmt.Errorf("OIDC is not enabled: %w", api.ErrUnauthorized)
		}
		return a.oidc.Authenticate(ctx, cred)
	default:
		return api.Identity{}, fmt.Errorf("unrecognized credential format: %w", api.ErrUnauthorized)
	}
}
