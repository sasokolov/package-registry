package config

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const cfgV1 = `
storage:
  type: fs
  fs: {path: /data}
feeds:
  - name: one
    format: echo
    anonymous: true
`

const cfgV2 = `
storage:
  type: fs
  fs: {path: /data}
feeds:
  - name: one
    format: echo
    anonymous: true
  - name: two
    format: echo
    anonymous: true
`

func writeConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func TestManagerReloadSwapsSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, path, cfgV1)

	m, err := NewManager(path, testLogger(), nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	var notified *Config
	m.Subscribe(func(c *Config) { notified = c })

	if got := len(m.Current().Feeds); got != 1 {
		t.Fatalf("initial feeds = %d, want 1", got)
	}

	writeConfig(t, path, cfgV2)
	if err := m.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := len(m.Current().Feeds); got != 2 {
		t.Errorf("feeds after reload = %d, want 2", got)
	}
	if notified == nil || len(notified.Feeds) != 2 {
		t.Error("subscriber was not notified with the new snapshot")
	}
}

func TestManagerKeepsOldSnapshotOnInvalidReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, path, cfgV1)

	m, err := NewManager(path, testLogger(), nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	old := m.Current()

	writeConfig(t, path, "storage: {type: nope}")
	if err := m.Reload(); err == nil {
		t.Fatal("Reload of invalid config succeeded, want error")
	}
	if m.Current() != old {
		t.Error("snapshot was swapped despite invalid config")
	}
}

func TestManagerValidateHookRejects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, path, cfgV1)

	reject := errors.New("format not registered")
	if _, err := NewManager(path, testLogger(), func(*Config) error { return reject }); !errors.Is(err, reject) {
		t.Fatalf("NewManager with failing validate = %v, want %v", err, reject)
	}

	m, err := NewManager(path, testLogger(), nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	m.validate = func(*Config) error { return reject }
	old := m.Current()
	writeConfig(t, path, cfgV2)
	if err := m.Reload(); !errors.Is(err, reject) {
		t.Fatalf("Reload = %v, want %v", err, reject)
	}
	if m.Current() != old {
		t.Error("snapshot swapped despite validate rejection")
	}
}

func TestManagerIntervalReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, path, "server:\n  reload_interval: 30ms\n"+cfgV1)

	m, err := NewManager(path, testLogger(), nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	runCtx, stop := context.WithCancel(t.Context())
	defer stop()
	done := make(chan struct{})
	go func() { m.Run(runCtx); close(done) }()

	writeConfig(t, path, "server:\n  reload_interval: 30ms\n"+cfgV2)

	deadline := time.After(3 * time.Second)
	for len(m.Current().Feeds) != 2 {
		select {
		case <-deadline:
			t.Fatal("interval reload did not pick up the new config in time")
		case <-time.After(10 * time.Millisecond):
		}
	}
	stop()
	<-done
}
