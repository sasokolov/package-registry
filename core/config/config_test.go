package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseValid(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want func(t *testing.T, cfg *Config)
	}{
		{
			name: "minimal fs config gets defaults",
			yaml: `
storage:
  type: fs
  fs:
    path: /data
`,
			want: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.Server.Listen != ":8080" {
					t.Errorf("default listen = %q, want :8080", cfg.Server.Listen)
				}
				if time.Duration(cfg.Server.ShutdownTimeout) != 10*time.Second {
					t.Errorf("default shutdown_timeout = %s, want 10s", cfg.Server.ShutdownTimeout)
				}
				if len(cfg.Feeds) != 0 {
					t.Errorf("feeds = %v, want empty", cfg.Feeds)
				}
			},
		},
		{
			name: "s3 storage",
			yaml: `
server:
  listen: "127.0.0.1:9999"
  shutdown_timeout: 1m30s
storage:
  type: s3
  s3:
    endpoint: minio:9000
    bucket: registry
    access_key: ak
    secret_key: sk
feeds: []
`,
			want: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.Server.Listen != "127.0.0.1:9999" {
					t.Errorf("listen = %q", cfg.Server.Listen)
				}
				if time.Duration(cfg.Server.ShutdownTimeout) != 90*time.Second {
					t.Errorf("shutdown_timeout = %s, want 1m30s", cfg.Server.ShutdownTimeout)
				}
				if cfg.Storage.S3.Bucket != "registry" {
					t.Errorf("bucket = %q", cfg.Storage.S3.Bucket)
				}
			},
		},
		{
			name: "feeds with and without upstream",
			yaml: `
storage:
  type: fs
  fs:
    path: /data
feeds:
  - name: maven-central
    format: maven
    upstream: https://repo1.maven.org/maven2
  - name: maven-hosted
    format: maven
`,
			want: func(t *testing.T, cfg *Config) {
				t.Helper()
				want := []FeedConfig{
					{Name: "maven-central", Format: "maven", Upstream: "https://repo1.maven.org/maven2"},
					{Name: "maven-hosted", Format: "maven"},
				}
				if !reflect.DeepEqual(cfg.Feeds, want) {
					t.Errorf("feeds = %+v, want %+v", cfg.Feeds, want)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse(strings.NewReader(tt.yaml))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			tt.want(t, cfg)
		})
	}
}

func TestParseInvalid(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string // substring that must appear in the error
	}{
		{
			name:    "empty input",
			yaml:    "",
			wantErr: "empty config",
		},
		{
			name:    "malformed yaml",
			yaml:    "server: [unclosed",
			wantErr: "parse yaml",
		},
		{
			name: "unknown top-level field",
			yaml: `
sarver:
  listen: ":8080"
storage:
  type: fs
  fs: {path: /data}
`,
			wantErr: "field sarver not found",
		},
		{
			name: "unknown nested field",
			yaml: `
storage:
  type: fs
  fs: {path: /data, compression: true}
`,
			wantErr: "field compression not found",
		},
		{
			name: "missing storage type",
			yaml: `
storage:
  fs: {path: /data}
`,
			wantErr: "storage.type is required",
		},
		{
			name: "unsupported storage type",
			yaml: `
storage:
  type: gcs
`,
			wantErr: `storage.type "gcs" is not supported`,
		},
		{
			name: "fs storage without path",
			yaml: `
storage:
  type: fs
`,
			wantErr: "storage.fs.path is required",
		},
		{
			name: "s3 storage without endpoint and bucket",
			yaml: `
storage:
  type: s3
`,
			wantErr: "storage.s3.endpoint is required",
		},
		{
			name: "bad listen address",
			yaml: `
server:
  listen: "no-port"
storage:
  type: fs
  fs: {path: /data}
`,
			wantErr: "server.listen",
		},
		{
			name: "shutdown_timeout not a duration",
			yaml: `
server:
  shutdown_timeout: fast
storage:
  type: fs
  fs: {path: /data}
`,
			wantErr: `invalid duration "fast"`,
		},
		{
			name: "feed without name",
			yaml: `
storage:
  type: fs
  fs: {path: /data}
feeds:
  - format: maven
`,
			wantErr: "feeds[0]: name is required",
		},
		{
			name: "feed name with invalid characters",
			yaml: `
storage:
  type: fs
  fs: {path: /data}
feeds:
  - name: Maven_Central
    format: maven
`,
			wantErr: "name must match",
		},
		{
			name: "duplicate feed names",
			yaml: `
storage:
  type: fs
  fs: {path: /data}
feeds:
  - name: central
    format: maven
  - name: central
    format: npm
`,
			wantErr: "duplicate feed name",
		},
		{
			name: "feed without format",
			yaml: `
storage:
  type: fs
  fs: {path: /data}
feeds:
  - name: central
`,
			wantErr: "format is required",
		},
		{
			name: "feed upstream with unsupported scheme",
			yaml: `
storage:
  type: fs
  fs: {path: /data}
feeds:
  - name: central
    format: maven
    upstream: ftp://repo1.maven.org
`,
			wantErr: `scheme "ftp" is not supported`,
		},
		{
			name: "feed upstream without host",
			yaml: `
storage:
  type: fs
  fs: {path: /data}
feeds:
  - name: central
    format: maven
    upstream: https:///just-a-path
`,
			wantErr: "URL has no host",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tt.yaml))
			if err == nil {
				t.Fatal("Parse succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateReportsAllErrors(t *testing.T) {
	in := `
server:
  listen: bad
storage:
  type: gcs
feeds:
  - name: UPPER
    format: ""
`
	_, err := Parse(strings.NewReader(in))
	if err == nil {
		t.Fatal("Parse succeeded, want error")
	}
	for _, want := range []string{"server.listen", "storage.type", "name must match", "format is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("joined error misses %q; got:\n%s", want, err)
		}
	}
}

func TestLoad(t *testing.T) {
	cfg, err := Load("testdata/valid.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Storage.Type != StorageFS {
		t.Errorf("storage.type = %q, want fs", cfg.Storage.Type)
	}
	if len(cfg.Feeds) != 1 || cfg.Feeds[0].Name != "maven-central" {
		t.Errorf("feeds = %+v", cfg.Feeds)
	}

	if _, err := Load("testdata/does-not-exist.yaml"); err == nil {
		t.Error("Load of missing file succeeded, want error")
	}
}
