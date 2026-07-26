// Package allowlist is the reference policy: it permits only coordinates
// whose package name matches one of the configured glob patterns.
//
// Feed config:
//
//	policies:
//	  - name: allowlist
//	    config:
//	      allow:
//	        - "org.apache.*"
//	        - "@myscope/*"
//
// Patterns use path.Match semantics against PackageCoordinate.Name ('*'
// does not cross '/'). An empty allow list denies everything — an explicit
// allowlist that lists nothing allows nothing.
package allowlist

import (
	"context"
	"fmt"
	"path"

	"github.com/sasokolov/package-registry/core/api"
)

// DenyCode is the machine-readable reason for allowlist denials.
const DenyCode = "not-in-allowlist"

func init() {
	api.RegisterPolicy("allowlist", New)
}

// Policy holds the compiled pattern list.
type Policy struct {
	patterns []string
}

// New builds the policy from its YAML options.
func New(options map[string]any, _ api.PolicyServices) (api.Policy, error) {
	rawList, ok := options["allow"]
	if !ok {
		return nil, fmt.Errorf("allowlist: option %q is required", "allow")
	}
	items, ok := rawList.([]any)
	if !ok {
		return nil, fmt.Errorf("allowlist: %q must be a list of glob strings", "allow")
	}
	p := &Policy{patterns: make([]string, 0, len(items))}
	for i, item := range items {
		s, ok := item.(string)
		if !ok || s == "" {
			return nil, fmt.Errorf("allowlist: allow[%d] must be a non-empty string", i)
		}
		// Validate the pattern eagerly: path.Match only errors on bad patterns.
		if _, err := path.Match(s, "probe"); err != nil {
			return nil, fmt.Errorf("allowlist: allow[%d] %q: %w", i, s, err)
		}
		p.patterns = append(p.patterns, s)
	}
	return p, nil
}

func (p *Policy) allowed(c api.PackageCoordinate) api.Decision {
	for _, pat := range p.patterns {
		if ok, _ := path.Match(pat, c.Name); ok {
			return api.Allowed()
		}
	}
	return api.Denied(DenyCode, fmt.Sprintf("package %s is not in the feed allowlist", c.String()))
}

// OnResolve implements api.Policy.
func (p *Policy) OnResolve(_ context.Context, _ api.Identity, c api.PackageCoordinate) api.Decision {
	return p.allowed(c)
}

// OnServe implements api.Policy.
func (p *Policy) OnServe(_ context.Context, _ api.Identity, a api.Artifact) api.Decision {
	return p.allowed(a.Coord)
}

// OnPublish implements api.Policy.
func (p *Policy) OnPublish(_ context.Context, _ api.Identity, a api.Artifact) api.Decision {
	return p.allowed(a.Coord)
}
