package license

import (
	"testing"

	"github.com/sasokolov/package-registry/core/api"
)

func mustPolicy(t *testing.T, options map[string]any) api.Policy {
	t.Helper()
	p, err := New(options, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func artifact(license string) api.Artifact {
	meta := map[string]string{}
	if license != "" {
		meta[api.MetaLicense] = license
	}
	return api.Artifact{
		Coord:    api.PackageCoordinate{Format: "maven", Name: "com.example:lib", Version: "1.0"},
		Metadata: meta,
	}
}

func TestLicenseDeny(t *testing.T) {
	p := mustPolicy(t, map[string]any{"deny": []any{"GPL-3.0", "AGPL-*"}})
	ctx := t.Context()

	tests := []struct {
		license string
		allow   bool
	}{
		{"Apache-2.0", true},
		{"GPL-3.0", false},
		{"gpl-3.0", false}, // SPDX ids are case-insensitive
		{"AGPL-3.0-only", false},
		{"MIT OR GPL-3.0", false}, // any denied part denies
		{"MIT OR Apache-2.0", true},
		{"", true}, // unknown allowed by default
	}
	for _, tt := range tests {
		d := p.OnServe(ctx, api.Anonymous(), artifact(tt.license))
		if d.Allow != tt.allow {
			t.Errorf("license %q: allow = %v, want %v", tt.license, d.Allow, tt.allow)
		}
		if !d.Allow && d.Code == "" {
			t.Errorf("license %q: deny without code", tt.license)
		}
	}

	// Publishing is gated the same way.
	if d := p.OnPublish(ctx, api.Anonymous(), artifact("GPL-3.0")); d.Allow {
		t.Error("OnPublish allowed a denied license")
	}
	// Resolution has no metadata yet, so it always passes.
	if d := p.OnResolve(ctx, api.Anonymous(), api.PackageCoordinate{Name: "x"}); !d.Allow {
		t.Error("OnResolve must not deny")
	}
}

func TestLicenseUnknownDeny(t *testing.T) {
	p := mustPolicy(t, map[string]any{"deny": []any{"GPL-3.0"}, "on_unknown": "deny"})
	d := p.OnServe(t.Context(), api.Anonymous(), artifact(""))
	if d.Allow {
		t.Fatal("unknown license allowed although on_unknown=deny")
	}
	if d.Code != DenyUnknownCode {
		t.Errorf("code = %q", d.Code)
	}
}

func TestLicenseBadOptions(t *testing.T) {
	for i, opts := range []map[string]any{
		{},
		{"deny": "not-a-list"},
		{"deny": []any{42}},
		{"deny": []any{"[bad"}},
		{"deny": []any{"MIT"}, "on_unknown": "maybe"},
	} {
		if _, err := New(opts, nil); err == nil {
			t.Errorf("case %d: bad options accepted: %v", i, opts)
		}
	}
}
