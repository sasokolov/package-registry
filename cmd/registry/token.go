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
	"github.com/sasokolov/package-registry/core/state"
)

// tokenCmd implements `registry token create -name <name> [-config <path>]`.
// The secret is printed once to stdout and only its hash is stored.
func tokenCmd(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "create" {
		return errors.New("usage: registry token create -name <name> [-config <path>]")
	}
	flags := flag.NewFlagSet("registry token create", flag.ContinueOnError)
	configPath := flags.String("config", "/etc/registry/config.yaml", "path to the YAML config file")
	name := flags.String("name", "", "unique token name (identity subject)")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("token create: -name is required")
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
