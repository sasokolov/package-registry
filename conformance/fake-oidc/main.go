// Command fake-oidc is a minimal OpenID provider for conformance runs and
// local development: a discovery document, a JWKS, an authorization endpoint
// with a consent screen, and a token endpoint that checks PKCE properly.
//
// Being strict is the point. A provider that redeemed any code for any
// verifier would let a broken registry pass — so this one refuses a mismatched
// verifier, redirect_uri or client_id, and says which, the way a real provider
// does when a client is wired up wrong.
//
// Standard library only, so it builds anywhere the registry does.
package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	key    *rsa.PrivateKey
	issuer string
	mu     sync.Mutex
	codes  = map[string]issued{}
	// subject is who this provider signs everybody in as. One identity is
	// enough to prove a registry accepts what it is given.
	subject = envOr("SUBJECT", "alice@example.com")
)

type issued struct {
	Challenge   string
	RedirectURI string
	Nonce       string
	ClientID    string
}

func main() {
	addr := envOr("ADDR", "127.0.0.1:9099")
	// ISSUER is how the registry reaches this provider, which inside a
	// compose network is not the address it listens on.
	issuer = envOr("ISSUER", "http://"+addr)

	var err error
	key, err = rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatal(err)
	}

	http.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                issuer,
			"authorization_endpoint":                issuer + "/authorize",
			"token_endpoint":                        issuer + "/token",
			"jwks_uri":                              issuer + "/jwks",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"code_challenge_methods_supported":      []string{"S256"},
		})
	})

	http.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		e := make([]byte, 8)
		binary.BigEndian.PutUint64(e, uint64(key.E)) //nolint:gosec
		writeJSON(w, map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "dev", "use": "sig", "alg": "RS256",
			"n": b64(key.N.Bytes()),
			"e": b64(trimLeadingZeros(e)),
		}}})
	})

	// A consent screen, so the flow looks like a real one in a browser.
	http.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Query().Get("approve") != "yes" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = fmt.Fprintf(w, `<!doctype html><meta charset=utf-8>
<title>Sign in — dev identity provider</title>
<body style="font:16px system-ui;max-width:32rem;margin:6rem auto">
<h1>dev identity provider</h1>
<p><b>%s</b> wants to sign you in as <code>%s</code>.</p>
<form><input type=hidden name=approve value=yes>%s
<button style="font:inherit;padding:.6rem 1.2rem">Approve</button></form>
</body>`, htmlEscape(q.Get("client_id")), subject, hidden(q))
			return
		}

		code := fmt.Sprintf("code-%d", time.Now().UnixNano())
		mu.Lock()
		codes[code] = issued{
			Challenge:   q.Get("code_challenge"),
			RedirectURI: q.Get("redirect_uri"),
			Nonce:       q.Get("nonce"),
			ClientID:    q.Get("client_id"),
		}
		mu.Unlock()
		log.Printf("authorized %s for %s -> %s", subject, q.Get("client_id"), q.Get("redirect_uri"))
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+q.Get("state"), http.StatusFound)
	})

	http.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		got, ok := codes[r.Form.Get("code")]
		delete(codes, r.Form.Get("code"))
		mu.Unlock()

		fail := func(why string) {
			log.Printf("token refused: %s", why)
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]string{"error": "invalid_grant", "error_description": why})
		}
		switch {
		case !ok:
			fail("no such code")
		case got.RedirectURI != r.Form.Get("redirect_uri"):
			fail("redirect_uri mismatch: issued for " + got.RedirectURI + ", presented " + r.Form.Get("redirect_uri"))
		case got.ClientID != r.Form.Get("client_id"):
			fail("client_id mismatch")
		case s256(r.Form.Get("code_verifier")) != got.Challenge:
			fail("code_verifier does not match the challenge")
		default:
			log.Printf("issued id_token for %s", subject)
			writeJSON(w, map[string]any{
				"token_type": "Bearer", "expires_in": 900,
				"access_token": "opaque",
				"id_token":     idToken(got.ClientID, got.Nonce),
			})
		}
	})

	log.Printf("fake identity provider: listening on %s, issuer %s, subject %s",
		addr, issuer, subject)
	server := &http.Server{Addr: addr, Handler: nil, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(server.ListenAndServe())
}

func idToken(audience, nonce string) string {
	now := time.Now()
	header := b64(mustJSON(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "dev"}))
	payload := b64(mustJSON(map[string]any{
		"iss": issuer, "aud": audience, "sub": subject,
		"iat": now.Unix(), "exp": now.Add(15 * time.Minute).Unix(),
		"nonce": nonce, "email": subject, "groups": []string{"platform"},
	}))
	signing := header + "." + payload
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		log.Fatal(err)
	}
	return signing + "." + b64(sig)
}

func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return b64(sum[:])
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func trimLeadingZeros(b []byte) []byte {
	for len(b) > 1 && b[0] == 0 {
		b = b[1:]
	}
	return b
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		log.Fatal(err)
	}
	return raw
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func hidden(q map[string][]string) string {
	var b strings.Builder
	for name, values := range q {
		for _, value := range values {
			fmt.Fprintf(&b, `<input type=hidden name=%q value=%q>`, name, value)
		}
	}
	return b.String()
}

func htmlEscape(s string) string {
	return strings.NewReplacer("<", "&lt;", ">", "&gt;", "&", "&amp;", `"`, "&quot;").Replace(s)
}
