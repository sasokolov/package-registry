package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwt"

	"github.com/sasokolov/package-registry/core/api"
	"github.com/sasokolov/package-registry/core/config"
)

// The browser half of the fake issuer: an authorization endpoint that hands
// out codes and a token endpoint that will only redeem one if the PKCE
// verifier, the redirect URI and the client all match what the code was
// issued for. Making the fake strict is the point — a lenient one would pass
// whatever this registry sent, including the wrong thing.

const testClientID = "registry-console"

type issuedCode struct {
	challenge   string
	redirectURI string
	clientID    string
	nonce       string
}

type browserFlow struct {
	mu    sync.Mutex
	codes map[string]issuedCode
	// lastBasicAuth records what the registry authenticated as, so a test
	// can prove a client secret was actually presented.
	lastBasicAuth [2]string
	// audience overrides what the issued id_token is addressed to.
	audience string
}

func (fi *fakeIssuer) mountBrowserFlow(mux *http.ServeMux) {
	fi.flow = &browserFlow{codes: map[string]issuedCode{}}

	mux.HandleFunc("/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		// A real provider shows a consent screen here; this one approves.
		q := r.URL.Query()
		code := fmt.Sprintf("code-%d", len(fi.flow.codes)+1)
		fi.flow.mu.Lock()
		fi.flow.codes[code] = issuedCode{
			challenge:   q.Get("code_challenge"),
			redirectURI: q.Get("redirect_uri"),
			clientID:    q.Get("client_id"),
			nonce:       q.Get("nonce"),
		}
		fi.flow.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+q.Get("state"),
			http.StatusFound)
	})

	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if user, pass, ok := r.BasicAuth(); ok {
			fi.flow.mu.Lock()
			fi.flow.lastBasicAuth = [2]string{user, pass}
			fi.flow.mu.Unlock()
		}

		fi.flow.mu.Lock()
		issued, known := fi.flow.codes[r.Form.Get("code")]
		fi.flow.mu.Unlock()

		reject := func(reason string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "invalid_grant", "error_description": reason,
			})
		}
		switch {
		case r.Form.Get("grant_type") != "authorization_code":
			reject("unsupported grant type")
			return
		case !known:
			reject("no such code")
			return
		case issued.redirectURI != r.Form.Get("redirect_uri"):
			reject("redirect_uri does not match the one the code was issued for")
			return
		case issued.clientID != r.Form.Get("client_id"):
			reject("client_id does not match")
			return
		case s256(r.Form.Get("code_verifier")) != issued.challenge:
			reject("code_verifier does not match the challenge")
			return
		}

		audience := fi.flow.audience
		if audience == "" {
			audience = issued.clientID
		}
		token := fi.sign(fi.t, func(b *jwt.Builder) {
			b.Audience([]string{audience})
			b.Subject("alice")
			b.Claim("nonce", issued.nonce)
		})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "opaque", "token_type": "Bearer", "id_token": token,
		})
	})
}

func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// browserOIDC builds a validator whose issuer is also an OAuth client.
func browserOIDC(t *testing.T, fi *fakeIssuer, mutate func(*config.OIDCIssuer)) *OIDC {
	t.Helper()
	cfg := config.OIDCIssuer{
		Issuer:   fi.server.URL,
		Audience: "package-registry",
		ClientID: testClientID,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return NewOIDC(t.Context(), []config.OIDCIssuer{cfg}, fi.server.Client())
}

// signIn drives the whole flow the way a browser would, and returns what the
// registry made of it.
func signIn(t *testing.T, fi *fakeIssuer, o *OIDC, verifier string) (Exchanged, error) {
	t.Helper()
	code := authorize(t, fi, redirectURI, s256(verifier), "n-1")
	return o.Exchange(t.Context(), ExchangeRequest{
		Issuer:      fi.server.URL,
		Code:        code,
		Verifier:    verifier,
		RedirectURI: redirectURI,
		Nonce:       "n-1",
	})
}

// authorize is the redirect a browser follows, reduced to the one thing that
// comes back from it.
func authorize(t *testing.T, fi *fakeIssuer, redirectURI, challenge, nonce string) string {
	t.Helper()
	client := fi.server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := client.Get(fi.server.URL + "/oauth/authorize?" + strings.NewReplacer(" ", "%20").Replace(
		"client_id="+testClientID+
			"&redirect_uri="+redirectURI+
			"&code_challenge="+challenge+
			"&nonce="+nonce+
			"&state=s-1"))
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	location := resp.Header.Get("Location")
	_, query, _ := strings.Cut(location, "?")
	for _, pair := range strings.Split(query, "&") {
		if name, value, ok := strings.Cut(pair, "="); ok && name == "code" {
			return value
		}
	}
	t.Fatalf("the issuer redirected to %q with no code", location)
	return ""
}

const redirectURI = "https://registry.example.com/ui/oidc/callback"

// The whole point: a person ends up holding a credential this registry
// accepts, without anybody pasting anything.
func TestBrowserSignInProducesAWorkingCredential(t *testing.T) {
	fi := newFakeIssuer(t)
	o := browserOIDC(t, fi, nil)

	result, err := signIn(t, fi, o, "verifier-that-is-long-enough-to-be-real-0123456789")
	if err != nil {
		t.Fatalf("sign-in: %v", err)
	}
	if result.IDToken == "" {
		t.Fatal("no id_token")
	}
	if result.Identity.Subject != "alice" || result.Identity.Kind != api.IdentityOIDC {
		t.Errorf("identity = %+v", result.Identity)
	}
	if result.Identity.Issuer != fi.server.URL {
		t.Errorf("issuer = %q", result.Identity.Issuer)
	}
	if result.ExpiresAt.IsZero() || result.ExpiresAt.Before(time.Now()) {
		t.Errorf("expiry = %v, want a time in the future", result.ExpiresAt)
	}

	// And the credential really is one: the ordinary path accepts it.
	id, err := o.Authenticate(t.Context(), result.IDToken)
	if err != nil {
		t.Fatalf("the credential a sign-in produced is not accepted: %v", err)
	}
	if id.Subject != "alice" {
		t.Errorf("identity = %+v", id)
	}
}

// One issuer serves both the pipelines and the console. The audience differs
// between the two — a pipeline picks its own, a browser sign-in gets the
// client_id — and requiring one would break the other.
func TestBothAudiencesAreAccepted(t *testing.T) {
	fi := newFakeIssuer(t)
	o := browserOIDC(t, fi, nil)

	fromCI := fi.sign(t, nil) // aud: package-registry
	if _, err := o.Authenticate(t.Context(), fromCI); err != nil {
		t.Errorf("a pipeline's id_token stopped working: %v", err)
	}

	fromBrowser := fi.sign(t, func(b *jwt.Builder) { b.Audience([]string{testClientID}) })
	if _, err := o.Authenticate(t.Context(), fromBrowser); err != nil {
		t.Errorf("a browser sign-in's id_token was refused: %v", err)
	}

	somebodyElses := fi.sign(t, func(b *jwt.Builder) { b.Audience([]string{"another-service"}) })
	if _, err := o.Authenticate(t.Context(), somebodyElses); !errors.Is(err, api.ErrUnauthorized) {
		t.Errorf("a token addressed elsewhere was accepted: %v", err)
	}
}

// PKCE is the whole protection for a public client: an intercepted code is
// worthless without the verifier that never left the browser.
func TestAStolenCodeIsUselessWithoutTheVerifier(t *testing.T) {
	fi := newFakeIssuer(t)
	o := browserOIDC(t, fi, nil)

	code := authorize(t, fi, redirectURI, s256("the-real-verifier-0123456789012345"), "n-1")
	_, err := o.Exchange(t.Context(), ExchangeRequest{
		Issuer:      fi.server.URL,
		Code:        code,
		Verifier:    "a-guess-0123456789012345678901234",
		RedirectURI: redirectURI,
		Nonce:       "n-1",
	})
	if !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("error = %v, want unauthorized", err)
	}
	if !strings.Contains(err.Error(), "code_verifier") {
		t.Errorf("the error does not say what the issuer objected to: %v", err)
	}
}

// The nonce ties the token to this sign-in. Without the check, a token
// obtained by some other means would be accepted as the answer to this flow.
func TestATokenFromAnotherSignInIsRefused(t *testing.T) {
	fi := newFakeIssuer(t)
	o := browserOIDC(t, fi, nil)

	verifier := "verifier-0123456789012345678901234567"
	code := authorize(t, fi, redirectURI, s256(verifier), "the-issuers-nonce")
	_, err := o.Exchange(t.Context(), ExchangeRequest{
		Issuer:      fi.server.URL,
		Code:        code,
		Verifier:    verifier,
		RedirectURI: redirectURI,
		Nonce:       "what-this-tab-asked-for",
	})
	if !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("error = %v, want unauthorized", err)
	}
	if !strings.Contains(err.Error(), "nonce") {
		t.Errorf("error = %v, want it to name the nonce", err)
	}
}

// Only the configuration decides where the registry posts a code. A caller
// that could name an issuer would have it post the client's credentials
// wherever it liked.
func TestOnlyConfiguredIssuersCanBeSignedInTo(t *testing.T) {
	fi := newFakeIssuer(t)
	o := browserOIDC(t, fi, nil)

	_, err := o.Exchange(t.Context(), ExchangeRequest{
		Issuer:      "https://attacker.example",
		Code:        "code-1",
		Verifier:    "whatever-0123456789012345678901234567",
		RedirectURI: redirectURI,
	})
	if !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("error = %v, want unauthorized", err)
	}
}

// A trusted issuer that is not a client of this registry cannot be redirected
// to, and the refusal says why rather than failing further along.
func TestAnIssuerWithNoClientIDOffersNoButton(t *testing.T) {
	fi := newFakeIssuer(t)
	o := newOIDC(t, fi, "")

	if issuers := o.BrowserIssuers(); len(issuers) != 0 {
		t.Errorf("BrowserIssuers = %v, want none", issuers)
	}
	_, err := o.Metadata(t.Context(), fi.server.URL)
	if !errors.Is(err, ErrNoBrowserSignIn) {
		t.Fatalf("error = %v, want ErrNoBrowserSignIn", err)
	}
}

// Endpoints come from discovery when the configuration does not spell them
// out, and from the configuration when it does — an issuer that publishes a
// wrong document has to be workable.
func TestEndpointsComeFromDiscoveryOrConfiguration(t *testing.T) {
	fi := newFakeIssuer(t)

	discovered, err := browserOIDC(t, fi, nil).Metadata(t.Context(), fi.server.URL)
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if discovered.AuthorizationEndpoint != fi.server.URL+"/oauth/authorize" {
		t.Errorf("authorization_endpoint = %q", discovered.AuthorizationEndpoint)
	}
	if discovered.TokenEndpoint != fi.server.URL+"/oauth/token" {
		t.Errorf("token_endpoint = %q", discovered.TokenEndpoint)
	}
	if len(discovered.Scopes) != 1 || discovered.Scopes[0] != "openid" {
		t.Errorf("scopes = %v, want the default openid", discovered.Scopes)
	}

	configured, err := browserOIDC(t, fi, func(c *config.OIDCIssuer) {
		c.AuthorizationEndpoint = "https://elsewhere.example/authorize"
		c.TokenEndpoint = "https://elsewhere.example/token"
		c.Scopes = []string{"openid", "email"}
	}).Metadata(t.Context(), fi.server.URL)
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if configured.AuthorizationEndpoint != "https://elsewhere.example/authorize" {
		t.Errorf("the configured endpoint was ignored: %q", configured.AuthorizationEndpoint)
	}
	if len(configured.Scopes) != 2 {
		t.Errorf("scopes = %v", configured.Scopes)
	}
}

// A client secret named in the configuration and missing from the process is
// a deployment mistake worth naming, not a mysterious refusal from the
// issuer.
func TestAMissingClientSecretIsReportedAsSuch(t *testing.T) {
	fi := newFakeIssuer(t)
	o := browserOIDC(t, fi, func(c *config.OIDCIssuer) {
		c.ClientSecretEnv = "REGISTRY_TEST_SECRET_THAT_IS_NOT_SET"
	})

	_, err := signIn(t, fi, o, "verifier-0123456789012345678901234567")
	if err == nil {
		t.Fatal("a sign-in worked without the configured secret")
	}
	if !strings.Contains(err.Error(), "REGISTRY_TEST_SECRET_THAT_IS_NOT_SET") {
		t.Errorf("the error does not name the variable: %v", err)
	}
	// And the secret's absence is reported, not its value — there is none to
	// leak, but the message must not become one later either.
	if strings.Contains(err.Error(), "code_verifier") {
		t.Errorf("the failure leaked flow material: %v", err)
	}
}

// A configured secret is presented the way RFC 6749 says: Basic, not a form
// field an issuer is free to ignore.
func TestAConfiguredClientSecretIsPresented(t *testing.T) {
	fi := newFakeIssuer(t)
	t.Setenv("REGISTRY_TEST_CLIENT_SECRET", "s3cr3t")
	o := browserOIDC(t, fi, func(c *config.OIDCIssuer) {
		c.ClientSecretEnv = "REGISTRY_TEST_CLIENT_SECRET"
	})

	if _, err := signIn(t, fi, o, "verifier-0123456789012345678901234567"); err != nil {
		t.Fatalf("sign-in: %v", err)
	}
	fi.flow.mu.Lock()
	defer fi.flow.mu.Unlock()
	if fi.flow.lastBasicAuth[0] != testClientID || fi.flow.lastBasicAuth[1] != "s3cr3t" {
		t.Errorf("the issuer saw %q as the client credentials", fi.flow.lastBasicAuth[0])
	}
}

// An id_token the registry would refuse must fail the sign-in, not be handed
// to the console to fail on its next request.
func TestATokenTheRegistryWouldRefuseFailsTheSignIn(t *testing.T) {
	fi := newFakeIssuer(t)
	fi.flow.audience = "some-other-service"
	o := browserOIDC(t, fi, nil)

	_, err := signIn(t, fi, o, "verifier-0123456789012345678901234567")
	if err == nil {
		t.Fatal("a token this registry does not accept was handed back as a credential")
	}
	if !strings.Contains(err.Error(), "not one this registry accepts") {
		t.Errorf("error = %v", err)
	}
}

// A discovery document that names a different issuer is a configuration
// mix-up: following it would mean trusting keys from somewhere else.
func TestADiscoveryDocumentThatNamesAnotherIssuerIsRefused(t *testing.T) {
	fi := newFakeIssuer(t)
	fi.claimsToBe = "https://not-this-issuer.example"
	o := browserOIDC(t, fi, nil)

	_, err := o.Metadata(t.Context(), fi.server.URL)
	if err == nil {
		t.Fatal("a document claiming another issuer was accepted")
	}
	if !strings.Contains(err.Error(), "not-this-issuer.example") {
		t.Errorf("error = %v", err)
	}
}
