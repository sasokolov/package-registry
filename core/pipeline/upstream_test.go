package pipeline

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sasokolov/package-registry/core/api"
)

// A 429 is the upstream working and asking for a lower rate. Treating it as a
// failure opens the circuit breaker, which turns "slow down" into "this feed
// is down" for every client — the exact opposite of what the upstream asked
// for, and how one busy build takes a proxy feed offline for everyone.
func TestThrottlingDoesNotOpenTheBreaker(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) <= 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("body"))
	}))
	defer server.Close()

	up, err := NewUpstream(UpstreamOptions{Feed: "central", BaseURL: server.URL, Retries: 5})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := up.Fetch(t.Context(), "a/b.jar", FetchOpts{})
	if err != nil {
		t.Fatalf("a throttled fetch never recovered: %v", err)
	}
	_ = resp.Body.Close()
	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want the two throttles retried", got)
	}
	if state := up.BreakerState(); state != int(BreakerClosed) {
		t.Errorf("breaker state = %d, want closed: a rate limit is not an outage", state)
	}
}

// An upstream that only ever throttles must still fail the request — the
// caller has stale copies and a cache to fall back on — but it must not take
// the breaker down with it.
func TestPersistentThrottlingFailsWithoutOpeningTheBreaker(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	up, err := NewUpstream(UpstreamOptions{Feed: "central", BaseURL: server.URL, Retries: 2})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := up.Fetch(t.Context(), "a/b.jar", FetchOpts{}); err == nil {
		t.Fatal("a permanently throttled upstream reported success")
	} else if !errors.Is(err, api.ErrUpstreamUnavailable) {
		t.Errorf("error = %v, want it to read as unavailable so stale can serve", err)
	}
	if state := up.BreakerState(); state != int(BreakerClosed) {
		t.Errorf("breaker state = %d, want closed", state)
	}
}

// Retry-After is the upstream's own answer to "when", so it beats our guess.
func TestRetryAfterIsHonoured(t *testing.T) {
	tests := []struct {
		header string
		want   time.Duration
	}{
		{"", 0},
		{"2", 2 * time.Second},
		{"0", 0},
		{"not a number", 0},
		// Capped: honouring an hour would hold a client's connection open
		// for an hour when there is a cache right here.
		{"86400", maxRetryAfter},
	}
	for _, tc := range tests {
		if got := retryAfter(tc.header); got != tc.want {
			t.Errorf("retryAfter(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
	// The HTTP-date form, rounded because it is computed from time.Now.
	future := time.Now().Add(3 * time.Second).UTC().Format(http.TimeFormat)
	if got := retryAfter(future); got < 2*time.Second || got > 3*time.Second {
		t.Errorf("retryAfter(date) = %v, want about 3s", got)
	}
}

// A 5xx is different: the upstream is not working, and that is exactly what
// the breaker is for.
func TestServerErrorsStillOpenTheBreaker(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	up, err := NewUpstream(UpstreamOptions{Feed: "central", BaseURL: server.URL, Retries: 5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := up.Fetch(t.Context(), "a/b.jar", FetchOpts{}); err == nil {
		t.Fatal("a broken upstream reported success")
	}
	if state := up.BreakerState(); state == int(BreakerClosed) {
		t.Error("the breaker stayed closed through repeated 502s")
	}
}
