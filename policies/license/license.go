// Package license denies artifacts whose declared license matches a
// configured deny list. The license is read from the canonical
// api.MetaLicense key that format modules provide, so this policy contains
// no format knowledge.
//
// Feed config:
//
//	policies:
//	  - name: license
//	    config:
//	      deny: ["GPL-3.0", "AGPL-*"]
//	      on_unknown: allow    # allow (default) | deny
package license

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/fondaco-dev/fondaco/core/api"
)

// Deny codes.
const (
	DenyCode        = "denied-license"
	DenyUnknownCode = "unknown-license"
)

func init() {
	api.RegisterPolicy("license", New)
}

// Policy denies configured licenses.
type Policy struct {
	deny        []string
	denyUnknown bool
}

// New builds the policy from YAML options.
func New(options map[string]any, _ api.PolicyServices) (api.Policy, error) {
	raw, ok := options["deny"]
	if !ok {
		return nil, fmt.Errorf("license: option %q is required", "deny")
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("license: %q must be a list of SPDX identifiers or globs", "deny")
	}
	p := &Policy{deny: make([]string, 0, len(items))}
	for i, item := range items {
		s, ok := item.(string)
		if !ok || s == "" {
			return nil, fmt.Errorf("license: deny[%d] must be a non-empty string", i)
		}
		if _, err := path.Match(s, "probe"); err != nil {
			return nil, fmt.Errorf("license: deny[%d] %q: %w", i, s, err)
		}
		p.deny = append(p.deny, s)
	}
	switch v, _ := options["on_unknown"].(string); v {
	case "", "allow":
	case "deny":
		p.denyUnknown = true
	default:
		return nil, fmt.Errorf("license: on_unknown must be \"allow\" or \"deny\", got %q", v)
	}
	return p, nil
}

func (p *Policy) check(a api.Artifact) api.Decision {
	declared := strings.TrimSpace(a.Meta(api.MetaLicense))
	if declared == "" {
		if p.denyUnknown {
			return api.Denied(DenyUnknownCode,
				fmt.Sprintf("%s declares no license and unknown licenses are denied", a.Coord))
		}
		return api.Allowed()
	}
	// "A OR B" (multiple <licenses> entries): deny if ANY part is denied.
	for _, part := range strings.Split(declared, " OR ") {
		part = strings.TrimSpace(part)
		for _, pattern := range p.deny {
			if matchLicense(pattern, part) {
				return api.Denied(DenyCode,
					fmt.Sprintf("license %q of %s is denied by this feed", part, a.Coord))
			}
		}
	}
	return api.Allowed()
}

// matchLicense compares case-insensitively: SPDX identifiers are
// case-insensitive and poms carry free-form names.
func matchLicense(pattern, license string) bool {
	ok, _ := path.Match(strings.ToLower(pattern), strings.ToLower(license))
	return ok
}

// OnResolve implements api.Policy: metadata is not available before the
// artifact is known, so resolution always passes.
func (p *Policy) OnResolve(context.Context, api.Identity, api.PackageCoordinate) api.Decision {
	return api.Allowed()
}

// OnServe implements api.Policy.
func (p *Policy) OnServe(_ context.Context, _ api.Identity, a api.Artifact) api.Decision {
	return p.check(a)
}

// OnPublish implements api.Policy.
func (p *Policy) OnPublish(_ context.Context, _ api.Identity, a api.Artifact) api.Decision {
	return p.check(a)
}
