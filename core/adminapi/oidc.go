package adminapi

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/fondaco-dev/fondaco/core/api"
	"github.com/fondaco-dev/fondaco/core/auth"
)

// Browser sign-in, the half that has to live on the server.
//
// The browser owns the parts that must not leave it — the PKCE verifier, the
// state, the nonce — and the registry owns the parts the browser must not be
// trusted with: which issuers exist, where their endpoints are, what client
// this registry is, and where the redirect is allowed to land. So the browser
// asks "start a sign-in at this issuer with this challenge" and gets back a
// URL, then comes back with a code and gets an id_token.
//
// No session is created. What the console ends up holding is an ordinary
// id_token, presented in an ordinary Authorization header, exactly like the
// one a pipeline presents — so revoking access at the issuer revokes the
// console too, and a replica that never saw the sign-in can serve the next
// request (invariant 3).

// consoleCallbackPath is where the issuer sends the browser back. It is a
// route of the console, not an API endpoint: the page that reads the code is
// the one that has the verifier.
const consoleCallbackPath = "/ui/oidc/callback"

// AuthorizeResponse is where to send the browser.
type AuthorizeResponse struct {
	AuthorizationURL string `json:"authorization_url"`
}

// ExchangeResponse is a completed sign-in.
type ExchangeResponse struct {
	IDToken string `json:"id_token"`
	// ExpiresAt is when the registry will stop accepting this credential,
	// so the console can ask for a new one before a request fails rather
	// than after.
	ExpiresAt string `json:"expires_at,omitempty"`
	Identity  string `json:"identity"`
	Subject   string `json:"subject"`
	Issuer    string `json:"issuer"`
}

// handleOIDCAuthorize builds the URL that starts a sign-in.
//
// It is anonymous, and it has to be: it is what somebody with no credential
// uses to get one. Everything it echoes back was either sent by the caller
// (their own state, nonce and challenge) or is this site's own configuration,
// so there is nothing here to leak.
func (s *Server) handleOIDCAuthorize(w http.ResponseWriter, r *http.Request) {
	oidc := s.oidc()
	if oidc == nil {
		s.writeError(w, http.StatusNotImplemented, "this site accepts no OIDC issuers")
		return
	}

	query := r.URL.Query()
	issuer := query.Get("issuer")
	if issuer == "" {
		s.writeError(w, http.StatusBadRequest, "issuer is required")
		return
	}
	challenge := query.Get("code_challenge")
	state := query.Get("state")
	nonce := query.Get("nonce")
	if challenge == "" || state == "" || nonce == "" {
		s.writeError(w, http.StatusBadRequest,
			"code_challenge, state and nonce are required; the browser generates all three")
		return
	}

	meta, err := oidc.Metadata(r.Context(), issuer)
	if err != nil {
		s.writeOIDCError(w, "start a sign-in at "+issuer, err)
		return
	}

	target, err := url.Parse(meta.AuthorizationEndpoint)
	if err != nil {
		s.writeError(w, http.StatusBadGateway,
			"the issuer's authorization endpoint is not a URL: "+meta.AuthorizationEndpoint)
		return
	}
	params := target.Query()
	params.Set("response_type", "code")
	params.Set("client_id", meta.ClientID)
	params.Set("redirect_uri", s.consoleRedirectURI(r))
	params.Set("scope", strings.Join(meta.Scopes, " "))
	params.Set("state", state)
	params.Set("nonce", nonce)
	params.Set("code_challenge", challenge)
	params.Set("code_challenge_method", "S256")
	target.RawQuery = params.Encode()

	s.logger.Info("browser sign-in started", "issuer", issuer)
	writeJSON(w, http.StatusOK, AuthorizeResponse{AuthorizationURL: target.String()})
}

// handleOIDCExchange turns the code the issuer sent back into a credential.
func (s *Server) handleOIDCExchange(w http.ResponseWriter, r *http.Request) {
	oidc := s.oidc()
	if oidc == nil {
		s.writeError(w, http.StatusNotImplemented, "this site accepts no OIDC issuers")
		return
	}

	var body struct {
		Issuer   string `json:"issuer"`
		Code     string `json:"code"`
		Verifier string `json:"code_verifier"`
		Nonce    string `json:"nonce"`
	}
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Issuer == "" || body.Code == "" || body.Verifier == "" {
		s.writeError(w, http.StatusBadRequest,
			"issuer, code and code_verifier are required")
		return
	}

	result, err := oidc.Exchange(r.Context(), auth.ExchangeRequest{
		Issuer:      body.Issuer,
		Code:        body.Code,
		Verifier:    body.Verifier,
		RedirectURI: s.consoleRedirectURI(r),
		Nonce:       body.Nonce,
	})
	if err != nil {
		// The code and the verifier are credentials for the length of one
		// exchange; neither the log nor the answer repeats them
		// (invariant 12).
		s.audit.Warn("browser sign-in failed", "issuer", body.Issuer, "error", err.Error())
		s.writeOIDCError(w, "complete the sign-in", err)
		return
	}

	s.audit.Info("browser sign-in",
		"issuer", result.Identity.Issuer,
		"subject", result.Identity.Subject)

	out := ExchangeResponse{
		IDToken:  result.IDToken,
		Identity: result.Identity.String(),
		Subject:  result.Identity.Subject,
		Issuer:   result.Identity.Issuer,
	}
	if !result.ExpiresAt.IsZero() {
		out.ExpiresAt = result.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	writeJSON(w, http.StatusOK, out)
}

// writeOIDCError reports a sign-in failure with the status that says whose
// problem it is: a credential the issuer refused is the caller's, an issuer
// this registry cannot reach is not.
func (s *Server) writeOIDCError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, auth.ErrNoBrowserSignIn):
		s.writeError(w, http.StatusBadRequest,
			"could not "+what+": "+err.Error()+
				" — set client_id on the issuer to offer a sign-in button")
	case errors.Is(err, api.ErrUnauthorized):
		s.writeError(w, http.StatusUnauthorized, "could not "+what+": "+err.Error())
	default:
		s.writeError(w, http.StatusBadGateway, "could not "+what+": "+err.Error())
	}
}

// consoleRedirectURI is where the issuer must send the browser back.
//
// It is computed here, in both halves of the flow, rather than accepted from
// the caller: a redirect URI a request could choose is how an authorization
// code ends up somewhere else. site.external_url decides when it is set,
// because that is the address a proxy publishes and the one registered at the
// issuer; otherwise the request's own origin is used, which is right whenever
// the console is reached directly.
func (s *Server) consoleRedirectURI(r *http.Request) string {
	if external := strings.TrimSuffix(s.manager.Current().Site.ExternalURL, "/"); external != "" {
		return external + consoleCallbackPath
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		scheme = forwarded
	}
	return scheme + "://" + r.Host + consoleCallbackPath
}

// oidc returns the current validator, or nil when none is configured.
func (s *Server) oidc() *auth.OIDC {
	if s.deps.OIDC == nil {
		return nil
	}
	o := s.deps.OIDC()
	if o == nil || !o.Enabled() {
		return nil
	}
	return o
}
