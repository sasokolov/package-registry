// Package state owns dynamic state in PostgreSQL: the pgx pool, embedded
// goose migrations and the advisory-lock helper used for cross-replica
// coordination (invariant 9: no Redis).
//
// The database is required for publish, token issuing and audit; the read
// path must keep working when it is down (invariant 7), so Open never
// blocks on connectivity and callers treat errors here as degradation, not
// fatal.
package state

import (
	"context"
	"embed"
	"fmt"
	"hash/fnv"
	"io/fs"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	gooselock "github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// DB wraps the connection pool.
type DB struct {
	pool   *pgxpool.Pool
	cfg    *pgxpool.Config
	logger *slog.Logger
}

// Open parses the DSN (empty DSN falls back to the standard PG* environment
// variables) and creates a lazy pool; no connection is attempted here.
func Open(ctx context.Context, dsn string, logger *slog.Logger) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database dsn: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	return &DB{pool: pool, cfg: cfg, logger: logger}, nil
}

// Pool exposes the underlying pool for core packages (auth, audit).
func (db *DB) Pool() *pgxpool.Pool { return db.pool }

// Close releases the pool.
func (db *DB) Close() { db.pool.Close() }

// Migrate applies embedded goose migrations, serialized across replicas via
// goose's Postgres session locker.
func (db *DB) Migrate(ctx context.Context) error {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("migrations fs: %w", err)
	}
	// A dedicated database/sql handle keeps goose independent of the pool.
	sqlDB := stdlib.OpenDB(*db.cfg.ConnConfig)
	defer func() { _ = sqlDB.Close() }()

	locker, err := gooselock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("migration locker: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, sub, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("migration provider: %w", err)
	}
	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	for _, r := range results {
		db.logger.Info("migration applied", "source", r.Source.Path, "duration", r.Duration)
	}
	return nil
}

// MigrateLoop retries Migrate with backoff until it succeeds or ctx is done.
// Startup must not depend on database availability (invariant 7), so the
// caller runs this in a goroutine.
func (db *DB) MigrateLoop(ctx context.Context) {
	backoff := time.Second
	for {
		err := db.Migrate(ctx)
		if err == nil {
			db.logger.Info("database migrations up to date")
			return
		}
		if ctx.Err() != nil {
			return
		}
		db.logger.Warn("migrations failed, will retry", "error", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// LockID maps a textual key onto the 64-bit advisory-lock keyspace.
func LockID(key string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key)) // fnv never errors
	return int64(h.Sum64())     //nolint:gosec // deliberate wraparound into the signed lock keyspace
}

// WithLock runs fn while holding the cross-replica advisory lock for key.
// The lock is session-scoped on a dedicated connection: if the connection
// dies, PostgreSQL releases the lock automatically.
func (db *DB) WithLock(ctx context.Context, key string, fn func(ctx context.Context) error) error {
	conn, err := db.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("advisory lock %q: acquire conn: %w", key, err)
	}
	defer conn.Release()

	id := LockID(key)
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", id); err != nil {
		return fmt.Errorf("advisory lock %q: %w", key, err)
	}
	defer func() {
		// Unlock even when ctx is already cancelled; a broken connection is
		// fine — releasing it drops the session lock server-side.
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", id); err != nil {
			db.logger.Warn("advisory unlock failed; connection will be discarded", "key", key, "error", err)
			conn.Conn().Close(unlockCtx) //nolint:errcheck // best effort: closing forces the server to drop the lock
		}
	}()
	return fn(ctx)
}

// Ping checks connectivity (used by integration tests and diagnostics).
func (db *DB) Ping(ctx context.Context) error { return db.pool.Ping(ctx) }
