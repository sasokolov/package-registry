package main

import (
	"io"
	"strings"
	"testing"
)

// Every repl subcommand must at least parse its flags: a duplicate flag
// panics at registration, which no unit test caught until a conformance run
// failed at startup.
func TestReplSubcommandsParseTheirFlags(t *testing.T) {
	subcommands := []string{
		"status", "peers", "conflicts", "resolve", "retry-parked",
		"resync", "backfill", "trust-reset", "quarantine", "release",
	}
	for _, sub := range subcommands {
		t.Run(sub, func(t *testing.T) {
			// A config that does not exist makes the command fail AFTER
			// flag registration, which is exactly the surface under test.
			err := replCmd([]string{sub, "-config", "/nonexistent/registry.yaml"}, io.Discard)
			if err == nil {
				t.Fatal("expected an error for a missing config")
			}
			if strings.Contains(err.Error(), "flag redefined") ||
				strings.Contains(err.Error(), "flag provided but not defined") {
				t.Fatalf("flag registration is broken: %v", err)
			}
		})
	}
}

func TestReplRejectsUnknownSubcommand(t *testing.T) {
	if err := replCmd([]string{"nonsense"}, io.Discard); err == nil {
		t.Fatal("unknown subcommand accepted")
	}
	if err := replCmd(nil, io.Discard); err == nil {
		t.Fatal("empty invocation accepted")
	}
}
