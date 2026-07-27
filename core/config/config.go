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
	// Admins may change the configuration through the admin API. Identity
	// patterns, exactly like a feed's publishers. Empty means the API is
	// read-only: a registry that ships with an open config endpoint would
	// be worse than one with none.
	Admins []string `yaml:"admins"`
	// ConfigSource declares where the configuration document lives.
	ConfigSource SourceConfig `yaml:"config_source"`
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
	// An explicit 0 disables the fallback, exactly as documented; the
	// default is applied at load time, so "unset" never reaches here.
	return a.StaleIdentityWindow.Std()
}

// RevocationSweepOrDefault is the eviction interval for revoked tokens.
func (a AuthConfig) RevocationSweepOrDefault() time.Duration {
	if a.RevocationSweep > 0 {
		return a.RevocationSweep.Std()
	}
	return 5 * time.Second
}

// UnmarshalYAML records whether stale_identity_window was given at all, so
// an explicit 0 (disable) is distinguishable from an omitted key (default).
func (a *AuthConfig) UnmarshalYAML(node *yaml.Node) error {
	type plain AuthConfig
	var raw plain
	if err := node.Decode(&raw); err != nil {
		return err
	}
	*a = AuthConfig(raw)
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == "stale_identity_window" {
			a.staleWindowSet = true
		}
	}
	return nil
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
	// staleWindowSet distinguishes an explicit 0 from an absent key.
	staleWindowSet bool `yaml:"-"`
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
	Issuer string `yaml:"issuer" json:"issuer"`
	// Audience is the required "aud" claim value.
	Audience string `yaml:"audience" json:"audience"`
	// JWKSURL overrides the JWKS endpoint; default <issuer>/oauth/discovery/keys
	// via OIDC discovery is derived by core/auth when empty.
	JWKSURL string `yaml:"jwks_url,omitempty" json:"jwks_url,omitempty"`
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

// SourceConfig declares where the one configuration document lives.
// The document is always replaced whole; the source only decides who can
// read and write it (invariant 8).
type SourceConfig struct {
	// Type is "file" (the default: the path the process was started with,
	// which is what a GitOps-delivered ConfigMap looks like) or "store"
	// (an object in the blob store, which every replica can read and any
	// replica can write — what the admin API needs).
	Type string `yaml:"type"`
	// Key is the object key when Type is "store".
	Key string `yaml:"key"`
}

// SourceTypeOrDefault is the effective source type.
func (c SourceConfig) SourceTypeOrDefault() string {
	if c.Type == "" {
		return "file"
	}
	return c.Type
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
	Name     string `yaml:"name" json:"name"`
	Format   string `yaml:"format" json:"format"`
	Upstream string `yaml:"upstream,omitempty" json:"upstream,omitempty"`
	// Anonymous allows unauthenticated reads from this feed. Default false.
	Anonymous bool `yaml:"anonymous,omitempty" json:"anonymous,omitempty"`
	// Hosted enables locally published packages on this feed (requires the
	// format module to implement the Hoster capability and a database).
	Hosted bool `yaml:"hosted,omitempty" json:"hosted,omitempty"`
	// Publishers lists identity patterns allowed to publish here, e.g.
	// "token:ci-bot", "project:group/*", "*". Empty = publishing disabled.
	Publishers []string `yaml:"publishers,omitempty" json:"publishers,omitempty"`
	// UpstreamRPS rate-limits requests to this feed's upstream. 0 = unlimited.
	UpstreamRPS float64 `yaml:"upstream_rps,omitempty" json:"upstream_rps,omitempty"`
	// Redirect serves cached artifacts as a 302 to a pre-signed storage URL
	// instead of streaming them, when the storage supports it. Only
	// redirect-safe protocols honour it (see api.RedirectSafe).
	Redirect bool `yaml:"redirect,omitempty" json:"redirect,omitempty"`
	// RedirectTTL bounds a pre-signed URL. Default 15m.
	RedirectTTL Duration `yaml:"redirect_ttl,omitempty" json:"redirect_ttl,omitempty"`
	// PublishPolicy is the feed's write model in a federation:
	// "forward:<site>" (write-affinity, the default model) or "local"
	// (symmetric active-active, conflicts resolved by rule K1).
	PublishPolicy string `yaml:"publish_policy,omitempty" json:"publish_policy,omitempty"`
	// ReplicationMode is "eager" (blobs replicate ahead of demand, the
	// durability watermark is a real RPO) or "lazy" (blobs fetched on
	// demand from peers). Default lazy.
	ReplicationMode string `yaml:"replication_mode,omitempty" json:"replication_mode,omitempty"`
	// PeerFallback lets the read path fetch missing hosted content from
	// peers, hiding replication lag from clients.
	PeerFallback bool `yaml:"peer_fallback,omitempty" json:"peer_fallback,omitempty"`
	// Policies is the ordered policy chain for this feed.
	Policies []PolicyConfig `yaml:"policies,omitempty" json:"policies,omitempty"`
	// Members makes this feed a group: a read-only view over other feeds of
	// the same format, in this order. Artifacts come from the first member
	// that has them; the documents that list what exists are merged, so a
	// hosted member cannot hide what the proxied one offers.
	Members []string `yaml:"members,omitempty" json:"members,omitempty"`
}

// IsGroup reports whether this feed is a group over other feeds.
func (f FeedConfig) IsGroup() bool { return len(f.Members) > 0 }

// PolicyConfig names a registered policy and carries its options verbatim.
type PolicyConfig struct {
	Name    string         `yaml:"name" json:"name"`
	Options map[string]any `yaml:"config,omitempty" json:"config,omitempty"`
}

// API converts the feed declaration to the canonical api.Feed passed to
// modules.
func (f FeedConfig) API() api.Feed {
	return api.Feed{
		Name: f.Name, Format: f.Format, Upstream: f.Upstream,
		Anonymous: f.Anonymous, Hosted: f.Hosted, Group: f.IsGroup(),
	}
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

// expandNode substitutes ${VAR} references from the environment inside
// already-parsed scalar values. It is how secrets reach the config without
// ever being written into it: the file stays a ConfigMap, the values come
// from a Secret mounted as environment variables, and hot reload keeps
// working because nothing rewrites the file out of band.
//
// Expansion happens AFTER parsing and only inside scalars, so a secret can
// never change the document's structure: a value containing a newline, an
// anchor marker or "anonymous: true" stays one string. An unset variable is
// an error rather than an empty string — a registry that silently starts
// with no S3 credentials is worse than one that refuses to start.
func expandNode(node *yaml.Node, missing *[]string) {
	if node == nil {
		return
	}
	if node.Kind == yaml.ScalarNode {
		node.Value = envRef.ReplaceAllStringFunc(node.Value, func(match string) string {
			name := match[2 : len(match)-1]
			value, ok := os.LookupEnv(name)
			if !ok {
				*missing = append(*missing, name)
				return match
			}
			return value
		})
		// The substituted text is a value, never markup: force a style
		// that cannot be reinterpreted if the node is ever re-encoded.
		if node.Style == 0 && strings.ContainsAny(node.Value, "\n\"'&*!|>%@`") {
			node.Style = yaml.DoubleQuotedStyle
		}
		return
	}
	for _, child := range node.Content {
		expandNode(child, missing)
	}
}

// envRef matches ${NAME} with a conventional environment-variable name.
var envRef = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

// Parse decodes a YAML config from r, applies defaults and validates it.
// Unknown fields are rejected.
func Parse(r io.Reader) (*Config, error) {
	var doc yaml.Node
	dec := yaml.NewDecoder(io.LimitReader(r, 8<<20))
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("empty config")
		}
		return nil, fmt.Errorf("parse yaml: %w", err)
	}

	var missing []string
	expandNode(&doc, &missing)
	if len(missing) > 0 {
		return nil, fmt.Errorf("config references unset environment variable(s): %s",
			strings.Join(missing, ", "))
	}

	// Re-encode the expanded document and decode it strictly: unknown
	// fields must still be rejected, and the encoder quotes every scalar
	// correctly, so a substituted secret cannot become markup on the way
	// back in.
	expanded, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, fmt.Errorf("re-encode config: %w", err)
	}
	strict := yaml.NewDecoder(bytes.NewReader(expanded))
	strict.KnownFields(true)
	var cfg Config
	if err := strict.Decode(&cfg); err != nil {
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
	// Distinguish "unset" from an explicit 0: only an unset window gets the
	// default, so `stale_identity_window: 0` really disables the fallback.
	if !c.Auth.staleWindowSet {
		c.Auth.StaleIdentityWindow = Duration(6 * time.Hour)
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
	switch c.ConfigSource.SourceTypeOrDefault() {
	case "file", "store":
	default:
		errs = append(errs, fmt.Errorf(
			"config_source.type %q is not supported (want \"file\" or \"store\")", c.ConfigSource.Type))
	}
	if c.ConfigSource.SourceTypeOrDefault() == "store" && c.Storage.Type == StorageFS {
		// A filesystem store is per-replica, so the document would differ
		// between them — the one thing a single source of truth must not do.
		errs = append(errs, errors.New(
			"config_source.type: store requires shared storage (s3); a filesystem store is per-replica"))
	}
	for i, pattern := range c.Admins {
		if strings.TrimSpace(pattern) == "" {
			errs = append(errs, fmt.Errorf("admins[%d]: empty pattern", i))
		}
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
		if feed.Upstream == "" && !feed.Hosted && !feed.IsGroup() {
			errs = append(errs, fmt.Errorf("%s: a feed needs an upstream, hosted: true, members, or a combination", at))
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

	errs = append(errs, c.validateGroups()...)

	return errors.Join(errs...)
}

// maxGroupDepth bounds how deep groups may nest. Nesting is useful — a
// "public" group of "internal" plus a proxy — but a request fanning out
// through an unbounded tree is a way to turn one client into many.
const maxGroupDepth = 5

// validateGroups checks the group graph as a whole: what a group may
// contain, that its members exist and agree on the format, and that
// following them terminates.
func (c *Config) validateGroups() []error {
	var errs []error

	byName := make(map[string]FeedConfig, len(c.Feeds))
	for _, f := range c.Feeds {
		byName[f.Name] = f
	}

	for i, feed := range c.Feeds {
		if !feed.IsGroup() {
			continue
		}
		at := fmt.Sprintf("feeds[%d] (%s)", i, feed.Name)

		// A group is a view, not a repository: it stores nothing and
		// accepts nothing. Allowing either would create a second answer to
		// "where did this artifact actually come from".
		if feed.Upstream != "" {
			errs = append(errs, fmt.Errorf("%s: a group cannot have an upstream; proxy through a member instead", at))
		}
		if feed.Hosted {
			errs = append(errs, fmt.Errorf("%s: a group cannot be hosted; publish to a member instead", at))
		}
		if len(feed.Publishers) > 0 {
			errs = append(errs, fmt.Errorf("%s: a group cannot have publishers; they belong on the hosted member", at))
		}

		// The format has to know how to merge the documents that list what
		// exists, or the group would quietly serve a subset of them.
		if module, ok := api.Format(feed.Format); ok {
			if _, mergeable := module.(api.GroupMerger); !mergeable {
				errs = append(errs, fmt.Errorf(
					"%s: format %q does not support groups: it cannot merge the documents that list versions, "+
						"so a group would hide what its members hold", at, feed.Format))
			}
		}

		seenMember := map[string]bool{}
		for j, member := range feed.Members {
			switch {
			case member == feed.Name:
				errs = append(errs, fmt.Errorf("%s: members[%d]: a group cannot contain itself", at, j))
				continue
			case seenMember[member]:
				errs = append(errs, fmt.Errorf("%s: members[%d]: %q is listed twice", at, j, member))
				continue
			}
			seenMember[member] = true

			target, ok := byName[member]
			if !ok {
				errs = append(errs, fmt.Errorf("%s: members[%d]: no feed named %q", at, j, member))
				continue
			}
			if target.Format != feed.Format {
				errs = append(errs, fmt.Errorf(
					"%s: members[%d]: %q is a %s feed; a group can only contain its own format (%s)",
					at, j, member, target.Format, feed.Format))
			}
		}

		if err := walkGroup(byName, feed, nil, 0); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", at, err))
		}
	}
	return errs
}

// walkGroup follows a group's members and reports a cycle or excessive
// nesting. path is the chain that led here, used to name the cycle.
func walkGroup(byName map[string]FeedConfig, feed FeedConfig, path []string, depth int) error {
	if depth > maxGroupDepth {
		return fmt.Errorf("groups nest deeper than %d levels (%s)",
			maxGroupDepth, strings.Join(append(path, feed.Name), " -> "))
	}
	for _, seen := range path {
		if seen == feed.Name {
			return fmt.Errorf("groups form a cycle: %s", strings.Join(append(path, feed.Name), " -> "))
		}
	}
	path = append(path, feed.Name)
	for _, member := range feed.Members {
		target, ok := byName[member]
		if !ok {
			continue // already reported as a missing member
		}
		if !target.IsGroup() {
			continue
		}
		if err := walkGroup(byName, target, path, depth+1); err != nil {
			return err
		}
	}
	return nil
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
