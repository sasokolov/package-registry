package quarantine

import (
	"testing"
	"time"

	"github.com/fondaco-dev/fondaco/core/api"
)

var now = time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

func mustPolicy(t *testing.T, options map[string]any) api.Policy {
	t.Helper()
	p, err := New(options, func() time.Time { return now })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func artifact(publishedAt string) api.Artifact {
	meta := map[string]string{}
	if publishedAt != "" {
		meta[api.MetaPublishedAt] = publishedAt
	}
	return api.Artifact{
		Coord:    api.PackageCoordinate{Format: "npm", Name: "left-pad", Version: "1.0.0"},
		Metadata: meta,
	}
}

func TestQuarantineAge(t *testing.T) {
	p := mustPolicy(t, map[string]any{"min_age": "24h"})
	ctx := t.Context()

	tests := []struct {
		name        string
		publishedAt string
		allow       bool
	}{
		{"old enough", now.Add(-48 * time.Hour).Format(time.RFC3339), true},
		{"exactly at the boundary", now.Add(-24 * time.Hour).Format(time.RFC3339), true},
		{"too fresh", now.Add(-1 * time.Hour).Format(time.RFC3339), false},
		{"published in the future", now.Add(time.Hour).Format(time.RFC3339), false},
		{"unknown allowed by default", "", true},
		{"unparsable allowed by default", "yesterday", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := p.OnServe(ctx, api.Anonymous(), artifact(tt.publishedAt))
			if d.Allow != tt.allow {
				t.Errorf("allow = %v, want %v (reason %q)", d.Allow, tt.allow, d.Reason)
			}
			if !d.Allow && d.Code != DenyCode {
				t.Errorf("code = %q", d.Code)
			}
		})
	}

	// Locally published packages are new by definition: the age gate must
	// not block publishing.
	if d := p.OnPublish(ctx, api.Anonymous(), artifact(now.Format(time.RFC3339))); !d.Allow {
		t.Error("OnPublish denied a fresh local publish")
	}
}

func TestQuarantineUnknownDeny(t *testing.T) {
	p := mustPolicy(t, map[string]any{"min_age": "24h", "on_unknown": "deny"})
	d := p.OnServe(t.Context(), api.Anonymous(), artifact(""))
	if d.Allow || d.Code != DenyUnknownCode {
		t.Errorf("decision = %+v, want deny with %q", d, DenyUnknownCode)
	}
}

func TestQuarantineBadOptions(t *testing.T) {
	for i, opts := range []map[string]any{
		{},
		{"min_age": 24},
		{"min_age": "soon"},
		{"min_age": "-1h"},
		{"min_age": "1h", "on_unknown": "maybe"},
	} {
		if _, err := New(opts, time.Now); err == nil {
			t.Errorf("case %d: bad options accepted: %v", i, opts)
		}
	}
}
