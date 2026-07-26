// Package policy implements the per-feed policy chain: policies run in
// declared order, the first Deny wins, an empty chain allows everything.
// Policies are modules registered in api's compile-time registry.
package policy

import (
	"context"
	"fmt"

	"github.com/sasokolov/package-registry/core/api"
	"github.com/sasokolov/package-registry/core/config"
)

type namedPolicy struct {
	name string
	p    api.Policy
}

// Chain is an ordered policy list; it implements api.Policy itself.
type Chain struct {
	policies []namedPolicy
}

// NewChain instantiates every configured policy via the registry.
func NewChain(cfgs []config.PolicyConfig) (*Chain, error) {
	c := &Chain{policies: make([]namedPolicy, 0, len(cfgs))}
	for _, pc := range cfgs {
		p, err := api.NewPolicy(pc.Name, pc.Options)
		if err != nil {
			return nil, fmt.Errorf("build policy chain: %w", err)
		}
		c.policies = append(c.policies, namedPolicy{name: pc.Name, p: p})
	}
	return c, nil
}

// Len reports the number of policies in the chain.
func (c *Chain) Len() int { return len(c.policies) }

func (c *Chain) decide(f func(api.Policy) api.Decision) api.Decision {
	for _, np := range c.policies {
		d := f(np.p)
		if !d.Allow {
			if d.Policy == "" {
				d.Policy = np.name
			}
			return d
		}
	}
	return api.Allowed()
}

// OnResolve implements api.Policy.
func (c *Chain) OnResolve(ctx context.Context, id api.Identity, coord api.PackageCoordinate) api.Decision {
	return c.decide(func(p api.Policy) api.Decision { return p.OnResolve(ctx, id, coord) })
}

// OnServe implements api.Policy.
func (c *Chain) OnServe(ctx context.Context, id api.Identity, a api.Artifact) api.Decision {
	return c.decide(func(p api.Policy) api.Decision { return p.OnServe(ctx, id, a) })
}

// OnPublish implements api.Policy.
func (c *Chain) OnPublish(ctx context.Context, id api.Identity, a api.Artifact) api.Decision {
	return c.decide(func(p api.Policy) api.Decision { return p.OnPublish(ctx, id, a) })
}
