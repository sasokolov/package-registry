package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// ReplicationConfig configures geo federation (docs/geo-replication.md).
// Secrets are referenced by file, never inlined (invariants 8 and 12).
type ReplicationConfig struct {
	Enabled bool `yaml:"enabled"`
	// InternalListen is the address of the replication-only listener. It is
	// mandatory when replication is enabled: the internal API must never
	// share the public listener (invariant 14).
	InternalListen string         `yaml:"internal_listen"`
	Auth           ReplAuthConfig `yaml:"auth"`
	// Topology is "mesh" in v1: every site talks to every peer directly, so
	// an event's origin is always the site that authenticated.
	Topology string       `yaml:"topology"`
	Peers    []PeerConfig `yaml:"peers"`
	// Nudge sends peers a "poll now" hint after local writes.
	Nudge bool `yaml:"nudge"`
	// JournalRetention bounds how long local journal entries are kept; a
	// peer whose cursor falls behind it re-bootstraps from a snapshot.
	JournalRetention Duration `yaml:"journal_retention"`
	// MaxClockSkew parks events whose timestamp is further in the future.
	MaxClockSkew Duration        `yaml:"max_clock_skew"`
	BlobFetch    BlobFetchConfig `yaml:"blob_fetch"`
}

// ReplAuthConfig is how peers authenticate to each other.
type ReplAuthConfig struct {
	// Type is "mtls" (production) or "bearer" (development).
	Type string `yaml:"type"`
	// CAFile, CertFile and KeyFile configure mTLS.
	CAFile   string `yaml:"ca_file"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	// TokenFile holds this site's bearer token; peers are configured with
	// their own token files.
	TokenFile string `yaml:"token_file"`
}

// PeerConfig declares one replication partner.
type PeerConfig struct {
	Name         string   `yaml:"name"`
	URL          string   `yaml:"url"`
	PullInterval Duration `yaml:"pull_interval"`
	// TokenFile is the credential presented to this peer (bearer mode).
	TokenFile string `yaml:"token_file"`
}

// BlobFetchConfig bounds peer blob transfers.
type BlobFetchConfig struct {
	Concurrency int  `yaml:"concurrency"`
	Resume      bool `yaml:"resume"`
}

// Validate checks the replication section.
func (r ReplicationConfig) Validate(siteName string) error {
	if !r.Enabled {
		return nil
	}
	var errs []error
	if r.InternalListen == "" {
		errs = append(errs, errors.New("replication.internal_listen is required when replication is enabled: the internal API must not share the public listener"))
	}
	switch r.Topology {
	case "", "mesh":
	default:
		errs = append(errs, fmt.Errorf("replication.topology %q is not supported (only \"mesh\" in this version)", r.Topology))
	}
	switch r.Auth.Type {
	case "mtls":
		for name, path := range map[string]string{
			"ca_file": r.Auth.CAFile, "cert_file": r.Auth.CertFile, "key_file": r.Auth.KeyFile,
		} {
			if path == "" {
				errs = append(errs, fmt.Errorf("replication.auth.%s is required for mtls", name))
				continue
			}
			if _, err := os.Stat(path); err != nil {
				errs = append(errs, fmt.Errorf("replication.auth.%s: %w", name, err))
			}
		}
	case "bearer":
		if r.Auth.TokenFile == "" {
			errs = append(errs, errors.New("replication.auth.token_file is required for bearer auth"))
		}
	case "":
		errs = append(errs, errors.New("replication.auth.type is required (mtls or bearer)"))
	default:
		errs = append(errs, fmt.Errorf("replication.auth.type %q is not supported", r.Auth.Type))
	}

	seen := map[string]bool{}
	for i, p := range r.Peers {
		at := fmt.Sprintf("replication.peers[%d]", i)
		if p.Name == "" {
			errs = append(errs, fmt.Errorf("%s: name is required", at))
		}
		if p.Name == siteName {
			errs = append(errs, fmt.Errorf("%s: a site cannot peer with itself", at))
		}
		if seen[p.Name] {
			errs = append(errs, fmt.Errorf("%s: duplicate peer name %q", at, p.Name))
		}
		seen[p.Name] = true
		if err := validateHTTPURL(p.URL); err != nil {
			errs = append(errs, fmt.Errorf("%s: url: %w", at, err))
		}
		if p.PullInterval < 0 {
			errs = append(errs, fmt.Errorf("%s: pull_interval must not be negative", at))
		}
	}
	if r.JournalRetention < 0 {
		errs = append(errs, errors.New("replication.journal_retention must not be negative"))
	}
	if r.MaxClockSkew < 0 {
		errs = append(errs, errors.New("replication.max_clock_skew must not be negative"))
	}
	return errors.Join(errs...)
}

// RetentionOrDefault is how long journal entries are kept.
func (r ReplicationConfig) RetentionOrDefault() time.Duration {
	if r.JournalRetention > 0 {
		return r.JournalRetention.Std()
	}
	return 30 * 24 * time.Hour
}

// SkewOrDefault is the tolerated clock skew for incoming events.
func (r ReplicationConfig) SkewOrDefault() time.Duration {
	if r.MaxClockSkew > 0 {
		return r.MaxClockSkew.Std()
	}
	return 5 * time.Minute
}

// ReadToken loads a bearer token from its file, trimming whitespace.
func ReadToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	return token, nil
}

// PublishPolicy describes where a feed accepts writes.
type PublishPolicy struct {
	// HomeSite is set when publishes must be forwarded to another site.
	HomeSite string
	// Local is true when this site accepts publishes directly (either the
	// feed has no home site, this site IS the home, or the feed opted into
	// symmetric active-active publishing).
	Local bool
	// ActiveActive is true for feeds that accept concurrent publishes at
	// several sites; conflicts are then resolved by rule K1.
	ActiveActive bool
}

// Publish returns the effective publish policy of a feed for this site.
//
//	publish_policy: forward:<site>   write-affinity (default when set)
//	publish_policy: local            symmetric active-active (opt-in)
//	unset                            local (single-site deployments)
func (f FeedConfig) Publish(siteName string) PublishPolicy {
	switch {
	case f.PublishPolicy == "" || f.PublishPolicy == "local":
		return PublishPolicy{Local: true, ActiveActive: f.PublishPolicy == "local"}
	case strings.HasPrefix(f.PublishPolicy, "forward:"):
		home := strings.TrimPrefix(f.PublishPolicy, "forward:")
		if home == siteName {
			return PublishPolicy{HomeSite: home, Local: true}
		}
		return PublishPolicy{HomeSite: home}
	default:
		return PublishPolicy{Local: true}
	}
}
