package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/sasokolov/package-registry/core/config"
	"github.com/sasokolov/package-registry/core/server"
)

// configCmd implements `registry config check -config <path>`: parse and
// fully validate a config without starting anything. It is what CI and a
// chart smoke test run to catch a config that would only fail at rollout,
// and what an operator runs before a SIGHUP.
func configCmd(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "check" {
		return errors.New("usage: registry config check [-config <path>]")
	}
	flags := flag.NewFlagSet("registry config check", flag.ContinueOnError)
	configPath := flags.String("config", "/etc/registry/config.yaml", "path to the YAML config file")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	// The same semantic pass the server applies: every referenced format,
	// storage and policy must actually be registered in this binary.
	if err := server.ValidateConfig(cfg); err != nil {
		return fmt.Errorf("config %s: %w", *configPath, err)
	}

	_, _ = fmt.Fprintf(out, "%s is valid: site %s, %d feed(s), storage %s, replication %v\n",
		*configPath, cfg.Site.Name, len(cfg.Feeds), cfg.Storage.Type, cfg.Replication.Enabled)
	return nil
}
