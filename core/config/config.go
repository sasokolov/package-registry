// Package config defines the declarative YAML configuration of the registry
// (schema v0: server, storage, feeds) and its loading and validation.
//
// Configuration is file-based only; it is never stored in the database.
// Hot-reload (SIGHUP/interval) is a Phase 1 task in PLAN.md.
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
)

// Storage backend types accepted in Config.Storage.Type.
const (
	StorageFS = "fs"
	StorageS3 = "s3"
)

// Config is the root of the registry configuration (schema v0).
type Config struct {
	Server  ServerConfig  `yaml:"server"`
	Storage StorageConfig `yaml:"storage"`
	Feeds   []FeedConfig  `yaml:"feeds"`
}

// ServerConfig configures the HTTP listener.
type ServerConfig struct {
	// Listen is the address to bind, e.g. ":8080". Default ":8080".
	Listen string `yaml:"listen"`
	// ShutdownTimeout bounds graceful shutdown. Default 10s.
	ShutdownTimeout Duration `yaml:"shutdown_timeout"`
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

// FeedConfig declares a single feed. In schema v0 only identity and the
// optional upstream are described; auth and policy settings arrive in Phase 1.
type FeedConfig struct {
	Name     string `yaml:"name"`
	Format   string `yaml:"format"`
	Upstream string `yaml:"upstream"`
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
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.Server.ShutdownTimeout == 0 {
		c.Server.ShutdownTimeout = Duration(10 * time.Second)
	}
}

// Validate checks the whole config and returns all violations joined into a
// single error.
func (c *Config) Validate() error {
	var errs []error

	if _, _, err := net.SplitHostPort(c.Server.Listen); err != nil {
		errs = append(errs, fmt.Errorf("server.listen %q is not a host:port address: %w", c.Server.Listen, err))
	}
	if c.Server.ShutdownTimeout < 0 {
		errs = append(errs, errors.New("server.shutdown_timeout must not be negative"))
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
			if err := validateUpstream(feed.Upstream); err != nil {
				errs = append(errs, fmt.Errorf("%s: upstream: %w", at, err))
			}
		}
	}

	return errors.Join(errs...)
}

func validateUpstream(raw string) error {
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
