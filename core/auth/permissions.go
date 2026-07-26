package auth

import (
	"fmt"
	"path"
	"strings"

	"github.com/sasokolov/package-registry/core/api"
)

// Publishers decides who may publish into a feed. Subjects are declared in
// the feed's YAML (invariant 8) as patterns matched against the identity:
//
//	token:<name>          static token, exact name or glob
//	oidc:<sub>            OIDC subject claim
//	project:<path>        GitLab project_path claim (the common case)
//	*                     any authenticated identity
//
// Patterns use path.Match semantics, so "project:group/*" works.
type Publishers struct {
	patterns []string
}

// NewPublishers compiles the per-feed publisher patterns.
func NewPublishers(patterns []string) (*Publishers, error) {
	p := &Publishers{patterns: make([]string, 0, len(patterns))}
	for i, raw := range patterns {
		if raw == "" {
			return nil, fmt.Errorf("publishers[%d] must not be empty", i)
		}
		if _, err := path.Match(raw, "probe"); err != nil {
			return nil, fmt.Errorf("publishers[%d] %q: %w", i, raw, err)
		}
		p.patterns = append(p.patterns, raw)
	}
	return p, nil
}

// Empty reports whether the feed declares no publishers (publishing off).
func (p *Publishers) Empty() bool { return p == nil || len(p.patterns) == 0 }

// Allowed reports whether id may publish.
func (p *Publishers) Allowed(id api.Identity) bool {
	if p.Empty() || id.IsAnonymous() {
		return false
	}
	for _, subject := range identitySubjects(id) {
		for _, pat := range p.patterns {
			if pat == "*" {
				return true
			}
			if ok, _ := path.Match(pat, subject); ok {
				return true
			}
		}
	}
	return false
}

// identitySubjects lists the strings an identity can be matched by.
func identitySubjects(id api.Identity) []string {
	subjects := []string{string(id.Kind) + ":" + id.Subject}
	if id.ProjectPath != "" {
		subjects = append(subjects, "project:"+id.ProjectPath)
	}
	if id.Ref != "" && id.ProjectPath != "" {
		subjects = append(subjects, "project:"+id.ProjectPath+"@"+id.Ref)
	}
	return subjects
}

// Describe renders the configured patterns for error messages.
func (p *Publishers) Describe() string {
	if p.Empty() {
		return "none"
	}
	return strings.Join(p.patterns, ", ")
}
