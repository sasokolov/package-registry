// Package quarantine denies package versions that are younger than a
// configured age — the window in which a compromised release is usually
// discovered and yanked. The publication time comes from the canonical
// api.MetaPublishedAt key, so the policy knows no formats.
//
// Feed config:
//
//	policies:
//	  - name: quarantine
//	    config:
//	      min_age: 24h
//	      on_unknown: allow   # allow (default) | deny
package quarantine

import (
	"context"
	"fmt"
	"time"

	"github.com/sasokolov/package-registry/core/api"
)

// Deny codes.
const (
	DenyCode        = "quarantined-new-release"
	DenyUnknownCode = "unknown-publication-time"
)

func init() {
	api.RegisterPolicy("quarantine", func(options map[string]any, _ api.PolicyServices) (api.Policy, error) {
		return New(options, time.Now)
	})
}

// Policy denies too-young releases.
type Policy struct {
	minAge      time.Duration
	denyUnknown bool
	now         func() time.Time
}

// New builds the policy; now is injectable for tests.
func New(options map[string]any, now func() time.Time) (api.Policy, error) {
	raw, ok := options["min_age"]
	if !ok {
		return nil, fmt.Errorf("quarantine: option %q is required", "min_age")
	}
	s, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("quarantine: min_age must be a duration string like \"24h\"")
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return nil, fmt.Errorf("quarantine: invalid min_age %q: %w", s, err)
	}
	if d < 0 {
		return nil, fmt.Errorf("quarantine: min_age must not be negative")
	}
	p := &Policy{minAge: d, now: now}
	switch v, _ := options["on_unknown"].(string); v {
	case "", "allow":
	case "deny":
		p.denyUnknown = true
	default:
		return nil, fmt.Errorf("quarantine: on_unknown must be \"allow\" or \"deny\", got %q", v)
	}
	return p, nil
}

func (p *Policy) check(a api.Artifact) api.Decision {
	raw := a.Meta(api.MetaPublishedAt)
	if raw == "" {
		if p.denyUnknown {
			return api.Denied(DenyUnknownCode,
				fmt.Sprintf("%s has no known publication time and unknown ages are denied", a.Coord))
		}
		return api.Allowed()
	}
	published, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		if p.denyUnknown {
			return api.Denied(DenyUnknownCode,
				fmt.Sprintf("%s has an unparsable publication time %q", a.Coord, raw))
		}
		return api.Allowed()
	}
	age := p.now().Sub(published)
	if age < p.minAge {
		return api.Denied(DenyCode, fmt.Sprintf(
			"%s was published %s ago; this feed quarantines releases younger than %s",
			a.Coord, age.Truncate(time.Minute), p.minAge))
	}
	return api.Allowed()
}

// OnResolve implements api.Policy.
func (p *Policy) OnResolve(context.Context, api.Identity, api.PackageCoordinate) api.Decision {
	return api.Allowed()
}

// OnServe implements api.Policy.
func (p *Policy) OnServe(_ context.Context, _ api.Identity, a api.Artifact) api.Decision {
	return p.check(a)
}

// OnPublish implements api.Policy: locally published packages are new by
// definition, so the age gate does not apply to them.
func (p *Policy) OnPublish(context.Context, api.Identity, api.Artifact) api.Decision {
	return api.Allowed()
}
