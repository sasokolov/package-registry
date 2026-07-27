// Package config defines the declarative YAML configuration of the registry
// (server, storage, database, auth, feeds) with loading, validation and
// hot-reload (SIGHUP + interval, see Manager).
//
// Configuration is file-based only; it is never stored in the database
// (invariant 8).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
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
	// Replication configures geo federation (docs/geo-replication.md).
	Replication ReplicationConfig `yaml:"replication"`
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

// ProjectionRepairOrDefault is how often the hosted-manifest projection is
// compared with the database.
func (s ServerConfig) ProjectionRepairOrDefault() time.Duration {
	if s.ProjectionRepair > 0 {
		return s.ProjectionRepair.Std()
	}
	return 5 * time.Minute
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
	// ProjectionRepair is how often the blob-store projection of hosted
	// manifests is compared with the database and repaired. Default 5m.
	ProjectionRepair Duration `yaml:"projection_repair"`
}

// DatabaseConfig configures PostgreSQL. An empty DSN disables the database
// entirely: static tokens, audit-to-db and publish are unavailable while
// reads keep working (invariant 7).
type DatabaseConfig struct {
	DSN string `yaml:"dsn"`
}

// StaleIdentityWindowOrDefault is how long a verified identity survives a
// token-backend outage. A negative value disables the fallback.
func (a AuthConfig) StaleIdentityWindowOrDefault() time.Duration {
	if a.StaleIdentityWindow != 0 {
		return a.StaleIdentityWindow.Std()
	}
	return 6 * time.Hour
}

// RevocationSweepOrDefault is the eviction interval for revoked tokens.
func (a AuthConfig) RevocationSweepOrDefault() time.Duration {
	if a.RevocationSweep > 0 {
		return a.RevocationSweep.Std()
	}
	return 5 * time.Second
}

// AuthConfig configures authentication.
type AuthConfig struct {
	// RevocationSweep is how often revoked tokens are evicted from the auth
	// cache. It bounds how long a revoked credential can still work, both
	// when revoked here and when the revocation arrives from a geo peer.
	RevocationSweep Duration `yaml:"revocation_sweep"`
	// StaleIdentityWindow is how long an already-verified identity may keep
	// working while the token backend is unreachable. It trades revocation
	// latency for read availability during a database outage (invariant 7);
	// 0 disables the fallback, so an outage past TokenCacheTTL degrades
	// loudly instead. Default 6h.
	StaleIdentityWindow Duration `yaml:"stale_identity_window"`
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
	Endpoint string `yaml:"endpoint"`
	Bucket   string `yaml:"bucket"`
	Region   string `yaml:"region"`
	// AccessKey and SecretKey may be given inline or, preferably, read from
	// files mounted from a Secret (invariant 12: secrets stay out of the
	// config document and out of logs).
	AccessKey     string `yaml:"access_key"`
	AccessKeyFile string `yaml:"access_key_file"`
	SecretKey     string `yaml:"secret_key"`
	SecretKeyFile string `yaml:"secret_key_file"`
	UseSSL        bool   `yaml:"use_ssl"`
}

// resolveSecrets loads any *_file credential into its inline field. It runs
// during Load, so the rest of the process never has to care which form was
// configured.
func (s *S3Config) resolveSecrets() error {
	for _, ref := range []struct {
		name  string
		file  string
		value *string
	}{
		{"access_key_file", s.AccessKeyFile, &s.AccessKey},
		{"secret_key_file", s.SecretKeyFile, &s.SecretKey},
	} {
		if ref.file == "" {
			continue
		}
		if *ref.value != "" {
			return fmt.Errorf("storage.s3: %s and its inline value are both set", ref.name)
		}
		raw, err := os.ReadFile(ref.file)
		if err != nil {
			return fmt.Errorf("storage.s3.%s: %w", ref.name, err)
		}
		secret := strings.TrimSpace(string(raw))
		if secret == "" {
			return fmt.Errorf("storage.s3.%s: file %s is empty", ref.name, ref.file)
		}
		*ref.value = secret
	}
	return nil
}

// FeedConfig declares a single feed.
type FeedConfig struct {
	Name     string `yaml:"name"`
	Format   string `yaml:"format"`
	Upstream string `yaml:"upstream"`
	// Anonymous allows unauthenticated reads from this feed. Default false.
	Anonymous bool `yaml:"anonymous"`
	// Hosted enables locally published packages on this feed (requires the
	// format module to implement the Hoster capability and a database).
	Hosted bool `yaml:"hosted"`
	// Publishers lists identity patterns allowed to publish here, e.g.
	// "token:ci-bot", "project:group/*", "*". Empty = publishing disabled.
	Publishers []string `yaml:"publishers"`
	// UpstreamRPS rate-limits requests to this feed's upstream. 0 = unlimited.
	UpstreamRPS float64 `yaml:"upstream_rps"`
	// Redirect serves cached artifacts as a 302 to a pre-signed storage URL
	// instead of streaming them, when the storage supports it. Only
	// redirect-safe protocols honour it (see api.RedirectSafe).
	Redirect bool `yaml:"redirect"`
	// RedirectTTL bounds a pre-signed URL. Default 15m.
	RedirectTTL Duration `yaml:"redirect_ttl"`
	// PublishPolicy is the feed's write model in a federation:
	// "forward:<site>" (write-affinity, the default model) or "local"
	// (symmetric active-active, conflicts resolved by rule K1).
	PublishPolicy string `yaml:"publish_policy"`
	// ReplicationMode is "eager" (blobs replicate ahead of demand, the
	// durability watermark is a real RPO) or "lazy" (blobs fetched on
	// demand from peers). Default lazy.
	ReplicationMode string `yaml:"replication_mode"`
	// PeerFallback lets the read path fetch missing hosted content from
	// peers, hiding replication lag from clients.
	PeerFallback bool `yaml:"peer_fallback"`
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

// RedirectTTLOrDefault is the pre-signed URL lifetime for this feed.
func (f FeedConfig) RedirectTTLOrDefault() time.Duration {
	if f.RedirectTTL > 0 {
		return f.RedirectTTL.Std()
	}
	return 15 * time.Minute
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

// expandEnv substitutes ${VAR} references from the environment. It is how
// secrets reach the config without ever being written into it: the file
// stays a ConfigMap, the values come from a Secret mounted as environment
// variables, and hot reload keeps working because nothing rewrites the file
// out of band. An unset variable is an error rather than an empty string —
// a registry that silently starts with no S3 credentials is worse than one
// that refuses to start.
func expandEnv(raw []byte) ([]byte, error) {
	var missing []string
	out := envRef.ReplaceAllFunc(raw, func(match []byte) []byte {
		name := string(match[2 : len(match)-1])
		value, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
			return match
		}
		return []byte(value)
	})
	if len(missing) > 0 {
		return nil, fmt.Errorf("config references unset environment variable(s): %s",
			strings.Join(missing, ", "))
	}
	return out, nil
}

// envRef matches ${NAME} with a conventional environment-variable name.
var envRef = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

// Parse decodes a YAML config from r, applies defaults and validates it.
// Unknown fields are rejected.
func Parse(r io.Reader) (*Config, error) {
	raw, err := io.ReadAll(io.LimitReader(r, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	expanded, err := expandEnv(raw)
	if err != nil {
		return nil, err
	}
	r = bytes.NewReader(expanded)

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
	// Credentials referenced by file are loaded before validation so the
	// rest of the process sees one uniform shape.
	if err := cfg.Storage.S3.resolveSecrets(); err != nil {
		return nil, err
	}
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
	if c.Auth.RevocationSweep == 0 {
		c.Auth.RevocationSweep = Duration(5 * time.Second)
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

	if err := c.Replication.Validate(c.Site.Name); err != nil {
		errs = append(errs, err)
	}
	// A feed homed elsewhere can only be published if we know where to
	// forward writes to.
	for _, feed := range c.Feeds {
		policy := feed.Publish(c.Site.Name)
		if policy.HomeSite == "" || policy.Local {
			continue
		}
		var known bool
		for _, p := range c.Replication.Peers {
			if p.Name == policy.HomeSite && p.PublicURL != "" {
				known = true
			}
		}
		if !known {
			errs = append(errs, fmt.Errorf(
				"feed %s is homed at site %q, but no peer with that name declares public_url: publishes could not be forwarded",
				feed.Name, policy.HomeSite))
		}
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
		if feed.RedirectTTL < 0 {
			errs = append(errs, fmt.Errorf("%s: redirect_ttl must not be negative", at))
		}
		if feed.UpstreamRPS < 0 {
			errs = append(errs, fmt.Errorf("%s: upstream_rps must not be negative", at))
		}
		if len(feed.Publishers) > 0 && !feed.Hosted {
			errs = append(errs, fmt.Errorf("%s: publishers require hosted: true", at))
		}
		if feed.Upstream == "" && !feed.Hosted {
			errs = append(errs, fmt.Errorf("%s: a feed needs an upstream, hosted: true, or both", at))
		}
		switch {
		case feed.PublishPolicy == "", feed.PublishPolicy == "local":
		case strings.HasPrefix(feed.PublishPolicy, "forward:"):
			if strings.TrimPrefix(feed.PublishPolicy, "forward:") == "" {
				errs = append(errs, fmt.Errorf("%s: publish_policy forward: needs a site name", at))
			}
		default:
			errs = append(errs, fmt.Errorf(
				"%s: publish_policy %q is not supported (want \"local\" or \"forward:<site>\")", at, feed.PublishPolicy))
		}
		switch feed.ReplicationMode {
		case "", "lazy", "eager":
		default:
			errs = append(errs, fmt.Errorf(
				"%s: replication_mode %q is not supported (want \"lazy\" or \"eager\")", at, feed.ReplicationMode))
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
