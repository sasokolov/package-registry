package config

import (
	"strings"
	"testing"
)

func TestInjectViaFeedUpstream(t *testing.T) {
	t.Setenv("NPM_UPSTREAM", "https://registry.npmjs.org\n    anonymous: true\n    hosted: true\n    publishers: [\"*\"]")
	cfg, err := Parse(strings.NewReader(`
site: {name: eu-1}
storage: {type: fs, fs: {path: /tmp/x}}
feeds:
  - name: private
    format: npm
    upstream: ${NPM_UPSTREAM}
`))
	if err != nil {
		t.Fatalf("Parse err: %v", err)
	}
	f := cfg.Feeds[0]
	t.Logf("feed %s anonymous=%v hosted=%v publishers=%v upstream=%q",
		f.Name, f.Anonymous, f.Hosted, f.Publishers, f.Upstream)
}

func TestInjectViaDSN(t *testing.T) {
	t.Setenv("PG_DSN", "postgres://u:p@h/db\nauth:\n  token_cache_ttl: 100h")
	cfg, err := Parse(strings.NewReader(`
site: {name: eu-1}
storage: {type: fs, fs: {path: /tmp/x}}
database:
  dsn: ${PG_DSN}
feeds:
  - name: npmjs
    format: npm
    upstream: https://registry.npmjs.org
`))
	if err != nil {
		t.Fatalf("Parse err: %v", err)
	}
	t.Logf("dsn=%q token_cache_ttl=%v", cfg.Database.DSN, cfg.Auth.TokenCacheTTL.Std())
}

func TestTrailingNewlineSecret(t *testing.T) {
	t.Setenv("S3_SECRET", "s3cr3t\n")
	cfg, err := Parse(strings.NewReader(`
site: {name: eu-1}
storage:
  type: s3
  s3:
    endpoint: minio:9000
    bucket: registry
    access_key: static
    secret_key: ${S3_SECRET}
feeds:
  - name: npmjs
    format: npm
    upstream: https://registry.npmjs.org
`))
	if err != nil {
		t.Logf("Parse err (fail-closed): %v", err)
		return
	}
	t.Logf("secret=%q", cfg.Storage.S3.SecretKey)
}
