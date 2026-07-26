package config

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"reflect"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Manager holds the current config snapshot and re-reads the file on SIGHUP
// and on an interval (invariant 8). A snapshot is immutable; consumers grab
// Current() per request. An invalid new file is rejected with a log record
// and the previous snapshot stays active.
type Manager struct {
	path     string
	logger   *slog.Logger
	validate func(*Config) error

	mu       sync.Mutex // serializes reloads and subscription
	subs     []func(*Config)
	lastHash [32]byte // content hash of the active snapshot
	cur      atomic.Pointer[Config]
}

// NewManager loads the initial config from path (failing hard on error —
// a process must not start with a broken config) and returns a manager.
// validate, if non-nil, adds semantic checks beyond Config.Validate (e.g.
// "are all referenced formats and policies registered").
func NewManager(path string, logger *slog.Logger, validate func(*Config) error) (*Manager, error) {
	m := &Manager{path: path, logger: logger, validate: validate}
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	if raw, err := os.ReadFile(path); err == nil {
		m.lastHash = sha256.Sum256(raw)
	}
	if validate != nil {
		if err := validate(cfg); err != nil {
			return nil, fmt.Errorf("config %s: %w", path, err)
		}
	}
	m.cur.Store(cfg)
	return m, nil
}

// Current returns the active immutable snapshot.
func (m *Manager) Current() *Config { return m.cur.Load() }

// Subscribe registers fn to be called synchronously after every successful
// reload. Must be called before Run.
func (m *Manager) Subscribe(fn func(*Config)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subs = append(m.subs, fn)
}

// Reload re-reads the file and atomically swaps the snapshot. On any error
// the previous snapshot stays active and the error is both logged and
// returned.
func (m *Manager) Reload() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	raw, readErr := os.ReadFile(m.path)
	if readErr == nil && sha256.Sum256(raw) == m.lastHash {
		// Unchanged file: nothing to swap, and no log line every interval.
		return nil
	}

	cfg, err := Load(m.path)
	if err == nil && m.validate != nil {
		if verr := m.validate(cfg); verr != nil {
			err = fmt.Errorf("config %s: %w", m.path, verr)
		}
	}
	if err != nil {
		m.logger.Error("config reload rejected, keeping previous snapshot", "error", err)
		return err
	}

	old := m.cur.Load()
	m.warnImmutableChanges(old, cfg)
	m.cur.Store(cfg)
	if readErr == nil {
		m.lastHash = sha256.Sum256(raw)
	}
	m.logger.Info("config reloaded", "path", m.path, "feeds", len(cfg.Feeds))
	for _, fn := range m.subs {
		fn(cfg)
	}
	return nil
}

// warnImmutableChanges reports sections that are only applied at startup.
func (m *Manager) warnImmutableChanges(old, next *Config) {
	if old == nil {
		return
	}
	if old.Server.Listen != next.Server.Listen {
		m.logger.Warn("server.listen changed; restart required to apply")
	}
	if !reflect.DeepEqual(old.Storage, next.Storage) {
		m.logger.Warn("storage section changed; restart required to apply")
	}
	if old.Database != next.Database {
		m.logger.Warn("database section changed; restart required to apply")
	}
}

// Run blocks until ctx is done, reloading on SIGHUP and every
// server.reload_interval (re-armed when a reload changes the interval).
func (m *Manager) Run(ctx context.Context) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	interval := time.Duration(m.Current().Server.ReloadInterval)
	var ticker *time.Ticker
	var tick <-chan time.Time
	if interval > 0 {
		ticker = time.NewTicker(interval)
		defer ticker.Stop()
		tick = ticker.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			m.logger.Info("SIGHUP received, reloading config")
			_ = m.Reload() // error already logged; old snapshot stays
		case <-tick:
			_ = m.Reload()
		}
		if ticker != nil {
			if next := time.Duration(m.Current().Server.ReloadInterval); next > 0 && next != interval {
				interval = next
				ticker.Reset(interval)
			}
		}
	}
}
