package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/sasokolov/package-registry/core/config"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func testMetricsRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	return reg
}

func TestRouterEndpoints(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	feeds := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	srv := httptest.NewServer(newRouter(&ready, func() bool { return true }, discardLogger(), testMetricsRegistry(), feeds))
	defer srv.Close()

	get := func(t *testing.T, path string) (int, string) {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		return resp.StatusCode, string(body)
	}

	if status, body := get(t, "/healthz"); status != http.StatusOK || body != "ok\n" {
		t.Errorf("/healthz = %d %q, want 200 ok", status, body)
	}
	if status, _ := get(t, "/readyz"); status != http.StatusOK {
		t.Errorf("/readyz = %d, want 200", status)
	}
	if status, body := get(t, "/metrics"); status != http.StatusOK || !strings.Contains(body, "go_goroutines") {
		t.Errorf("/metrics = %d, missing go_goroutines", status)
	}

	ready.Store(false)
	if status, _ := get(t, "/readyz"); status != http.StatusServiceUnavailable {
		t.Errorf("/readyz after shutdown start = %d, want 503", status)
	}
	if status, _ := get(t, "/healthz"); status != http.StatusOK {
		t.Errorf("/healthz must stay 200 while draining, got %d", status)
	}

	// Unmatched paths fall through to the feed handler.
	if status, _ := get(t, "/echo/test/some/file"); status != http.StatusTeapot {
		t.Errorf("feed fallthrough status = %d, want 418", status)
	}
}

func TestServeShutsDownGracefully(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Listen:          "127.0.0.1:0",
			ShutdownTimeout: config.Duration(5 * time.Second),
		},
	}
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- serveHTTP(ctx, cfg, discardLogger(), testMetricsRegistry(), nil, func() bool { return true })
	}()

	// Give the listener a moment to start, then trigger shutdown.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned error on graceful shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after context cancellation")
	}
}

// A pod whose migrations have not completed must not be routed to: the
// publish path depends on schema objects the newest migration creates.
func TestReadyzWaitsForMigrations(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	migrated := atomic.Bool{}

	srv := httptest.NewServer(newRouter(&ready, migrated.Load, discardLogger(), testMetricsRegistry(), nil))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("/readyz before migrations = %d, want 503", resp.StatusCode)
	}
	if !strings.Contains(string(body), "migrations") {
		t.Errorf("/readyz body does not say why: %q", body)
	}

	// Liveness is unaffected: the process is healthy, just not ready.
	resp, err = srv.Client().Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz before migrations = %d, want 200", resp.StatusCode)
	}

	migrated.Store(true)
	resp, err = srv.Client().Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/readyz after migrations = %d, want 200", resp.StatusCode)
	}
}
