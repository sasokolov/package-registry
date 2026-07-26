package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/sasokolov/package-registry/core/auth"
	"github.com/sasokolov/package-registry/core/config"
	"github.com/sasokolov/package-registry/core/repl"
	"github.com/sasokolov/package-registry/core/state"
)

// tokenCmd implements `registry token create|revoke -name <name>`.
// A created secret is printed once to stdout and only its hash is stored;
// a revocation is replicated to every geo site (invariant 14: replication
// may remove authority, never grant it).
func tokenCmd(args []string, out io.Writer) error {
	if len(args) == 0 || (args[0] != "create" && args[0] != "revoke") {
		return errors.New("usage: registry token create|revoke -name <name> [-config <path>]")
	}
	sub := args[0]
	flags := flag.NewFlagSet("registry token "+sub, flag.ContinueOnError)
	configPath := flags.String("config", "/etc/registry/config.yaml", "path to the YAML config file")
	name := flags.String("name", "", "unique token name (identity subject)")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("token %s: -name is required", sub)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	db, err := state.Open(ctx, cfg.Database.DSN, logger)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		return fmt.Errorf("prepare database: %w", err)
	}

	if sub == "revoke" {
		return revokeToken(ctx, db, cfg, *name, out)
	}

	secret, err := auth.NewTokens(db).Create(ctx, *name)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Token created. The secret is shown once and stored only as a hash:")
	if _, err := fmt.Fprintln(out, secret); err != nil {
		return fmt.Errorf("print token: %w", err)
	}
	return nil
}

// revokeToken revokes a token and, when replication is enabled, journals the
// revocation in the same transaction so peers cannot miss it.
func revokeToken(ctx context.Context, db *state.DB, cfg *config.Config, name string, out io.Writer) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	hash, err := auth.RevokeTx(ctx, tx, name)
	if err != nil {
		return err
	}
	if cfg.Replication.Enabled {
		if err := repl.NewWriter(cfg.Site.Name).AppendTokenRevoke(ctx, tx, hash); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit revocation: %w", err)
	}
	// Only the hash prefix is ever printed or logged (invariant 12).
	_, _ = fmt.Fprintf(out, "token %s revoked (hash %s…)\n", name, hash[:8])
	return nil
}
