package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwt"

	"github.com/sasokolov/package-registry/core/api"
)

// Signing in through the browser: OAuth 2.0 authorization code with PKCE.
//
// The console is a public client and holds no secret, so the browser starts
// the flow and the registry finishes it. The exchange runs here rather than
// in the browser for two reasons: it does not depend on the issuer allowing
// cross-origin calls to its token endpoint, which many do not, and it lets an
// issuer that insists on a confidential client work without the secret ever
// reaching a page.
//
// Nothing is remembered between the two halves. The browser holds the state
// and the PKCE verifier and sends the verifier back; the registry looks up
// the issuer in its own configuration and talks to it. That is what keeps the
// replicas stateless (invariant 3): the redirect may come back to a different
// pod than the one that started the flow, and neither has to have heard of
// the other.

// ProviderMetadata is what a browser needs to start a sign-in.
type ProviderMetadata struct {
	Issuer                string
	AuthorizationEndpoint string
	TokenEndpoint         string
	ClientID              string
	Scopes                []string
}

// Exchanged is a completed sign-in.
type Exchanged struct {
	// IDToken is the credential the console will present, exactly as a
	// pipeline presents its own.
	IDToken string
	// ExpiresAt is the id_token's own expiry, read from the token rather
	// than from the response envelope: it is the moment the registry will
	// stop accepting it, which is what the console needs to know.
	ExpiresAt time.Time
	// Identity is who the issuer said this is, already validated the same
	// way any other request's credential is.
	Identity api.Identity
}

// ErrNoBrowserSignIn means the issuer is trusted but has no client_id, so it
// can only be used by pasting a token something else issued.
var ErrNoBrowserSignIn = errors.New("this issuer is not configured for browser sign-in")

// BrowserIssuers lists the issuers a person can sign in through, in
// configuration order.
func (o *OIDC) BrowserIssuers() []ProviderMetadata {
	var out []ProviderMetadata
	for _, v := range o.ordered {
		if !v.cfg.BrowserSignIn() {
			continue
		}
		out = append(out, ProviderMetadata{
			Issuer:   v.cfg.Issuer,
			ClientID: v.cfg.ClientID,
			Scopes:   v.cfg.ScopesOrDefault(),
			// Endpoints are resolved on demand: discovery is a network call,
			// and a login form must render while the issuer is unreachable
			// — saying "sign-in is unavailable" beats not rendering at all.
			AuthorizationEndpoint: v.cfg.AuthorizationEndpoint,
			TokenEndpoint:         v.cfg.TokenEndpoint,
		})
	}
	return out
}

// Metadata resolves what a browser needs to start a sign-in at one issuer,
// consulting the issuer's discovery document for whatever the configuration
// did not spell out.
func (o *OIDC) Metadata(ctx context.Context, issuer string) (ProviderMetadata, error) {
	v, ok := o.issuers[issuer]
	if !ok {
		return ProviderMetadata{}, fmt.Errorf("issuer %q is not trusted: %w", issuer, api.ErrUnauthorized)
	}
	if !v.cfg.BrowserSignIn() {
		return ProviderMetadata{}, ErrNoBrowserSignIn
	}

	meta := ProviderMetadata{
		Issuer:                v.cfg.Issuer,
		ClientID:              v.cfg.ClientID,
		Scopes:                v.cfg.ScopesOrDefault(),
		AuthorizationEndpoint: v.cfg.AuthorizationEndpoint,
		TokenEndpoint:         v.cfg.TokenEndpoint,
	}
	if meta.AuthorizationEndpoint != "" && meta.TokenEndpoint != "" {
		return meta, nil
	}

	doc, err := o.discover(ctx, v)
	if err != nil {
		return ProviderMetadata{}, err
	}
	if meta.AuthorizationEndpoint == "" {
		meta.AuthorizationEndpoint = doc.AuthorizationEndpoint
	}
	if meta.TokenEndpoint == "" {
		meta.TokenEndpoint = doc.TokenEndpoint
	}
	if meta.AuthorizationEndpoint == "" || meta.TokenEndpoint == "" {
		return ProviderMetadata{}, fmt.Errorf(
			"issuer %s publishes no authorization_endpoint or token_endpoint; set them in "+
				"auth.oidc_issuers", issuer)
	}
	return meta, nil
}

// Exchange turns an authorization code into an id_token.
//
// The issuer is looked up in the configuration and never taken from the
// request beyond its name: a caller that could name an arbitrary token
// endpoint would have the registry post its client credentials wherever it
// liked.
func (o *OIDC) Exchange(ctx context.Context, req ExchangeRequest) (Exchanged, error) {
	issuer, code, verifier, redirectURI := req.Issuer, req.Code, req.Verifier, req.RedirectURI
	meta, err := o.Metadata(ctx, issuer)
	if err != nil {
		return Exchanged{}, err
	}
	v := o.issuers[issuer]

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {meta.ClientID},
		"code_verifier": {verifier},
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, meta.TokenEndpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return Exchanged{}, fmt.Errorf("token request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("Accept", "application/json")

	if name := v.cfg.ClientSecretEnv; name != "" {
		secret := os.Getenv(name)
		if secret == "" {
			return Exchanged{}, fmt.Errorf(
				"issuer %s is configured with client_secret_env %s, but that variable is "+
					"empty in this process", issuer, name)
		}
		// Basic is what RFC 6749 requires an authorization server to
		// accept; the form field is only optional there.
		httpReq.SetBasicAuth(url.QueryEscape(meta.ClientID), url.QueryEscape(secret))
	}

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return Exchanged{}, fmt.Errorf("token endpoint %s: %w", meta.TokenEndpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Exchanged{}, fmt.Errorf("token endpoint %s: read: %w", meta.TokenEndpoint, err)
	}

	var payload struct {
		IDToken          string `json:"id_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	// A non-JSON body from an authorization server is not worth quoting: it
	// is usually an HTML error page, and the status says as much.
	_ = json.Unmarshal(body, &payload)

	if resp.StatusCode != http.StatusOK {
		return Exchanged{}, fmt.Errorf("%w: the issuer refused the sign-in: %s",
			api.ErrUnauthorized, oauthError(resp.StatusCode, payload.Error, payload.ErrorDescription))
	}
	if payload.IDToken == "" {
		return Exchanged{}, fmt.Errorf(
			"the issuer returned no id_token; the client must request the openid scope and " +
				"the issuer must be an OpenID provider, not a bare OAuth server")
	}

	// The token is validated here, before it is handed back, so a sign-in
	// that would be refused on the next request fails now with a reason
	// instead of leaving the console holding something unusable.
	id, err := o.Authenticate(ctx, payload.IDToken)
	if err != nil {
		return Exchanged{}, fmt.Errorf("the issuer's id_token is not one this registry accepts: %w", err)
	}

	// The nonce ties the token to the sign-in that asked for it. Without it
	// a token obtained elsewhere — replayed from another session, or
	// injected by a page that could reach this endpoint — would be accepted
	// as the answer to this flow.
	if req.Nonce != "" {
		if err := checkNonce(payload.IDToken, req.Nonce); err != nil {
			return Exchanged{}, err
		}
	}

	return Exchanged{IDToken: payload.IDToken, ExpiresAt: expiryOf(payload.IDToken), Identity: id}, nil
}

// ExchangeRequest is one completed browser sign-in, as the browser reports it.
type ExchangeRequest struct {
	// Issuer names which configured issuer this is; everything else about
	// the issuer comes from the configuration, never from the caller.
	Issuer string
	// Code is the authorization code the issuer redirected back with.
	Code string
	// Verifier is the PKCE code_verifier whose challenge started the flow.
	Verifier string
	// RedirectURI must be the one the flow started with, and is computed by
	// the registry in both places rather than taken from the caller.
	RedirectURI string
	// Nonce is what the browser asked the issuer to bind into the token.
	Nonce string
}

// checkNonce verifies the token was minted for this sign-in.
func checkNonce(raw, want string) error {
	tok, err := jwt.ParseInsecure([]byte(raw))
	if err != nil {
		return fmt.Errorf("id_token could not be re-read to check its nonce: %w", err)
	}
	got, _ := stringClaim(tok, "nonce")
	if got != want {
		return fmt.Errorf(
			"%w: the id_token was not minted for this sign-in (nonce mismatch)", api.ErrUnauthorized)
	}
	return nil
}

// oauthError renders whatever the authorization server said about itself.
func oauthError(status int, code, description string) string {
	switch {
	case code != "" && description != "":
		return fmt.Sprintf("%s (%s)", description, code)
	case code != "":
		return code
	default:
		return fmt.Sprintf("status %d", status)
	}
}

// expiryOf reads exp without verifying anything; the token has already been
// verified by the time this is called.
func expiryOf(raw string) time.Time {
	tok, err := jwt.ParseInsecure([]byte(raw))
	if err != nil {
		return time.Time{}
	}
	return tok.Expiration()
}
