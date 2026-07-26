// Package config defines the declarative YAML configuration of the registry
// (server, storage, database, auth, feeds) with loading, validation and
// hot-reload (SIGHUP + interval, see Manager).
//
// Configuration is file-based only; it is never stored in the database
// (invariant 8).
package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sasokolov/package-registry/core/api"
)

// Storage backend types accepted in Config.Storage.Type.
const (
	StorageFS = "fs"
	StorageS3 = "s3"
)

// Config is the root of the registry configuration.
type Config struct {
	Site     SiteConfig     `yaml:"site"`
	Server   ServerConfig   `yaml:"server"`
	Storage  StorageConfig  `yaml:"storage"`
	Database DatabaseConfig `yaml:"database"`
	Auth     AuthConfig     `yaml:"auth"`
	Feeds    []FeedConfig   `yaml:"feeds"`
	// Replication is reserved for Phase 7 (geo federation, see
	// docs/geo-replication.md). It is accepted and ignored so configs
	// prepared for federation never crash older binaries during rolling
	// upgrades (strict parsing would otherwise reject the key).
	Replication map[string]any `yaml:"replication"`
}

// SiteConfig identifies this geo-site (docs/geo-replication.md). Single-site
// deployments keep the default.
type SiteConfig struct {
	// Name is a stable site identifier. Default "default".
	Name string `yaml:"name"`
	// ExternalURL is the public base URL of this site (optional until
	// federation).
	ExternalURL string `yaml:"external_url"`
}

// ServerConfig configures the HTTP listener.
type ServerConfig struct {
	// Listen is the address to bind, e.g. ":8080". Default ":8080".
	Listen string `yaml:"listen"`
	// ShutdownTimeout bounds graceful shutdown. Default 10s.
	ShutdownTimeout Duration `yaml:"shutdown_timeout"`
	// ReloadInterval is how often the config file is re-read in addition to
	// SIGHUP. 0 disables interval reloading. Default 30s.
	ReloadInterval Duration `yaml:"reload_interval"`
}

// DatabaseConfig configures PostgreSQL. An empty DSN disables the database
// entirely: static tokens, audit-to-db and publish are unavailable while
// reads keep working (invariant 7).
type DatabaseConfig struct {
	DSN string `yaml:"dsn"`
}

// AuthConfig configures authentication.
type AuthConfig struct {
	// TokenCacheTTL bounds the in-memory cache of verified static tokens;
	// within the TTL reads keep working while PostgreSQL is down
	// (invariant 7). Default 5m.
	TokenCacheTTL Duration     `yaml:"token_cache_ttl"`
	OIDC          []OIDCIssuer `yaml:"oidc_issuers"`
}

// OIDCIssuer declares a trusted OIDC issuer (e.g. a GitLab instance) whose
// id_tokens are accepted as identities.
type OIDCIssuer struct {
	// Issuer is the expected "iss" claim, e.g. "https://gitlab.com".
	Issuer string `yaml:"issuer"`
	// Audience is the required "aud" claim value.
	Audience string `yaml:"audience"`
	// JWKSURL overrides the JWKS endpoint; default <issuer>/oauth/discovery/keys
	// via OIDC discovery is derived by core/auth when empty.
	JWKSURL string `yaml:"jwks_url"`
}

// StorageConfig selects and configures the blob storage backend.
type StorageConfig struct {
	// Type is one of StorageFS, StorageS3.
	Type string   `yaml:"type"`
	FS   FSConfig `yaml:"fs"`
	S3   S3Config `yaml:"s3"`
}

// FSConfig configures filesystem blob storage.
type FSConfig struct {
	Path string `yaml:"path"`
}

// S3Config configures S3-compatible blob storage.
type S3Config struct {
	Endpoint  string `yaml:"endpoint"`
	Bucket    string `yaml:"bucket"`
	Region    string `yaml:"region"`
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	UseSSL    bool   `yaml:"use_ssl"`
}

// FeedConfig declares a single feed.
type FeedConfig struct {
	Name     string `yaml:"name"`
	Format   string `yaml:"format"`
	Upstream string `yaml:"upstream"`
	// Anonymous allows unauthenticated reads from this feed. Default false.
	Anonymous bool `yaml:"anonymous"`
	// UpstreamRPS rate-limits requests to this feed's upstream. 0 = unlimited.
	UpstreamRPS float64 `yaml:"upstream_rps"`
	// Policies is the ordered policy chain for this feed.
	Policies []PolicyConfig `yaml:"policies"`
}

// PolicyConfig names a registered policy and carries its options verbatim.
type PolicyConfig struct {
	Name    string         `yaml:"name"`
	Options map[string]any `yaml:"config"`
}

// API converts the feed declaration to the canonical api.Feed passed to
// modules.
func (f FeedConfig) API() api.Feed {
	return api.Feed{Name: f.Name, Format: f.Format, Upstream: f.Upstream, Anonymous: f.Anonymous}
}

// Options returns the active storage backend's options as a generic map for
// api.NewStorage.
func (s StorageConfig) Options() map[string]any {
	switch s.Type {
	case StorageFS:
		return map[string]any{"path": s.FS.Path}
	case StorageS3:
		return map[string]any{
			"endpoint":   s.S3.Endpoint,
			"bucket":     s.S3.Bucket,
			"region":     s.S3.Region,
			"access_key": s.S3.AccessKey,
			"secret_key": s.S3.SecretKey,
			"use_ssl":    s.S3.UseSSL,
		}
	default:
		return nil
	}
}

var (
	feedNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	formatRE   = regexp.MustCompile(`^[a-z0-9]+$`)
)

// Load reads, parses and validates the configuration file at path.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer func() { _ = f.Close() }() // read-only file, close error carries no information
	cfg, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// Parse decodes a YAML config from r, applies defaults and validates it.
// Unknown fields are rejected.
func Parse(r io.Reader) (*Config, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("empty config")
		}
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Site.Name == "" {
		c.Site.Name = "default"
	}
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.Server.ShutdownTimeout == 0 {
		c.Server.ShutdownTimeout = Duration(10 * time.Second)
	}
	if c.Server.ReloadInterval == 0 {
		c.Server.ReloadInterval = Duration(30 * time.Second)
	}
	if c.Auth.TokenCacheTTL == 0 {
		c.Auth.TokenCacheTTL = Duration(5 * time.Minute)
	}
}

// Validate checks the whole config and returns all violations joined into a
// single error.
func (c *Config) Validate() error {
	var errs []error

	if !feedNameRE.MatchString(c.Site.Name) {
		errs = append(errs, fmt.Errorf("site.name %q must match %s", c.Site.Name, feedNameRE))
	}
	if c.Site.ExternalURL != "" {
		if err := validateHTTPURL(c.Site.ExternalURL); err != nil {
			errs = append(errs, fmt.Errorf("site.external_url: %w", err))
		}
	}
	if _, _, err := net.SplitHostPort(c.Server.Listen); err != nil {
		errs = append(errs, fmt.Errorf("server.listen %q is not a host:port address: %w", c.Server.Listen, err))
	}
	if c.Server.ShutdownTimeout < 0 {
		errs = append(errs, errors.New("server.shutdown_timeout must not be negative"))
	}
	if c.Server.ReloadInterval < 0 {
		errs = append(errs, errors.New("server.reload_interval must not be negative"))
	}
	if c.Auth.TokenCacheTTL < 0 {
		errs = append(errs, errors.New("auth.token_cache_ttl must not be negative"))
	}

	for i, iss := range c.Auth.OIDC {
		at := fmt.Sprintf("auth.oidc_issuers[%d]", i)
		if err := validateHTTPURL(iss.Issuer); err != nil {
			errs = append(errs, fmt.Errorf("%s: issuer: %w", at, err))
		}
		if iss.Audience == "" {
			errs = append(errs, fmt.Errorf("%s: audience is required", at))
		}
		if iss.JWKSURL != "" {
			if err := validateHTTPURL(iss.JWKSURL); err != nil {
				errs = append(errs, fmt.Errorf("%s: jwks_url: %w", at, err))
			}
		}
	}

	switch c.Storage.Type {
	case "":
		errs = append(errs, errors.New("storage.type is required"))
	case StorageFS:
		if c.Storage.FS.Path == "" {
			errs = append(errs, errors.New("storage.fs.path is required for storage.type \"fs\""))
		}
	case StorageS3:
		if c.Storage.S3.Endpoint == "" {
			errs = append(errs, errors.New("storage.s3.endpoint is required for storage.type \"s3\""))
		}
		if c.Storage.S3.Bucket == "" {
			errs = append(errs, errors.New("storage.s3.bucket is required for storage.type \"s3\""))
		}
	default:
		errs = append(errs, fmt.Errorf("storage.type %q is not supported (want %q or %q)", c.Storage.Type, StorageFS, StorageS3))
	}

	seen := make(map[string]bool, len(c.Feeds))
	for i, feed := range c.Feeds {
		at := fmt.Sprintf("feeds[%d]", i)
		if feed.Name != "" {
			at = fmt.Sprintf("feeds[%d] (%s)", i, feed.Name)
		}
		switch {
		case feed.Name == "":
			errs = append(errs, fmt.Errorf("%s: name is required", at))
		case !feedNameRE.MatchString(feed.Name):
			errs = append(errs, fmt.Errorf("%s: name must match %s", at, feedNameRE))
		case seen[feed.Name]:
			errs = append(errs, fmt.Errorf("%s: duplicate feed name", at))
		default:
			seen[feed.Name] = true
		}
		switch {
		case feed.Format == "":
			errs = append(errs, fmt.Errorf("%s: format is required", at))
		case !formatRE.MatchString(feed.Format):
			errs = append(errs, fmt.Errorf("%s: format must match %s", at, formatRE))
		}
		if feed.Upstream != "" {
			if err := validateHTTPURL(feed.Upstream); err != nil {
				errs = append(errs, fmt.Errorf("%s: upstream: %w", at, err))
			}
		}
		if feed.UpstreamRPS < 0 {
			errs = append(errs, fmt.Errorf("%s: upstream_rps must not be negative", at))
		}
		for j, pol := range feed.Policies {
			if pol.Name == "" {
				errs = append(errs, fmt.Errorf("%s: policies[%d]: name is required", at, j))
			}
		}
	}

	return errors.Join(errs...)
}

func validateHTTPURL(raw string) error {
	if raw == "" {
		return errors.New("URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q is not supported (want http or https)", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("URL has no host")
	}
	return nil
}
