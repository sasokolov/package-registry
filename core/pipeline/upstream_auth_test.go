package pipeline

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Some registries publish everything anonymously and still refuse every
// request until it carries a token they hand out for free. Without the
// handshake a proxy of one of them fetches nothing at all — not a degraded
// answer, nothing.
func TestAnUpstreamThatDemandsATokenIsAnsweredWithOne(t *testing.T) {
	var tokensIssued, authorized, refused atomic.Int32

	var registry *httptest.Server
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The scope the challenge named has to travel with the request, or
		// the token is for nothing.
		if r.URL.Query().Get("scope") != "repository:library/alpine:pull" {
			http.Error(w, "no scope", http.StatusBadRequest)
			return
		}
		tokensIssued.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "issued-token", "expires_in": 300})
	}))
	defer tokenServer.Close()

	registry = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer issued-token" {
			refused.Add(1)
			w.Header().Set("WWW-Authenticate",
				`Bearer realm="`+tokenServer.URL+`",service="registry",scope="repository:library/alpine:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		authorized.Add(1)
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		_, _ = w.Write([]byte(`{"schemaVersion":2}`))
	}))
	defer registry.Close()

	up, err := NewUpstream(UpstreamOptions{Feed: "hub", BaseURL: registry.URL})
	if err != nil {
		t.Fatal(err)
	}

	body, contentType, err := up.FetchAll(t.Context(), "v2/library/alpine/manifests/3.20", FetchOpts{})
	if err != nil {
		t.Fatalf("the handshake failed: %v", err)
	}
	if string(body) != `{"schemaVersion":2}` {
		t.Errorf("body = %q", body)
	}
	if contentType != "application/vnd.oci.image.manifest.v1+json" {
		t.Errorf("content type = %q; a client dispatches on it", contentType)
	}

	// The second fetch reuses the token: a handshake per request would
	// triple the traffic to an upstream that is already rate limiting us.
	if _, _, err := up.FetchAll(t.Context(), "v2/library/alpine/manifests/3.21", FetchOpts{}); err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if got := tokensIssued.Load(); got != 1 {
		t.Errorf("%d tokens issued, want the first one reused", got)
	}
	if got := refused.Load(); got != 1 {
		t.Errorf("%d requests were refused, want only the very first", got)
	}
	if got := authorized.Load(); got != 2 {
		t.Errorf("%d authorized requests, want both", got)
	}
}

// A 401 that does not say where to get a token is just a 401. Guessing —
// retrying, or treating it as an outage — would turn a clear "you may not
// read this" into a circuit breaker opening for everyone else.
func TestA401WithoutAUsableChallengeStaysA401(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="private"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	up, err := NewUpstream(UpstreamOptions{Feed: "hub", BaseURL: server.URL, Retries: 2})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = up.FetchAll(t.Context(), "v2/private/manifests/1.0", FetchOpts{})
	if err == nil {
		t.Fatal("a refused fetch reported success")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v, want it to name the status", err)
	}
}

// A realm pointing somewhere internal is an upstream telling us to make a
// request on its behalf. The same destination check applies as to every
// other location an upstream hands us.
func TestATokenRealmIsSubjectToTheSSRFGuard(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="http://169.254.169.254/latest/meta-data/"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	up, err := NewUpstream(UpstreamOptions{Feed: "hub", BaseURL: server.URL, Retries: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := up.FetchAll(t.Context(), "v2/thing/manifests/1.0", FetchOpts{}); err == nil {
		t.Fatal("a token was fetched from a link-local address")
	}
}

// What a client says it understands is part of the request for protocols
// that answer one URL with several documents.
func TestTheAcceptHeaderReachesTheUpstream(t *testing.T) {
	var seen atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Header.Get("Accept"))
		_, _ = w.Write([]byte("{}"))
	}))
	defer server.Close()

	up, err := NewUpstream(UpstreamOptions{Feed: "hub", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	want := "application/vnd.oci.image.index.v1+json"
	if _, _, err := up.FetchAll(t.Context(), "v2/a/manifests/1", FetchOpts{Accept: want}); err != nil {
		t.Fatal(err)
	}
	if got, _ := seen.Load().(string); got != want {
		t.Errorf("upstream saw Accept %q, want %q", got, want)
	}
}

func TestChallengeParsing(t *testing.T) {
	ch := parseChallenge(`Bearer realm="https://auth.example/token",service="registry.example",scope="repository:a/b:pull,push"`)
	if ch.scheme != "bearer" {
		t.Errorf("scheme = %q", ch.scheme)
	}
	if ch.params["realm"] != "https://auth.example/token" {
		t.Errorf("realm = %q", ch.params["realm"])
	}
	// The scope legitimately contains a comma; splitting on every comma
	// would cut it in half and ask for half the rights.
	if ch.params["scope"] != "repository:a/b:pull,push" {
		t.Errorf("scope = %q", ch.params["scope"])
	}
}
