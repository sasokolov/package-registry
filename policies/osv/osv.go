// Package osv denies packages with known vulnerabilities, using the public
// OSV.dev API. Verdicts are cached in PostgreSQL with a TTL so builds stay
// fast and an OSV outage does not turn into an outage here.
//
// Feed config:
//
//	policies:
//	  - name: osv
//	    config:
//	      mode: warn          # warn (default) | enforce
//	      fail_open: true     # default true: OSV unreachable -> allow
//	      cache_ttl: 24h
//	      api_url: https://api.osv.dev/v1/query
//
// The default (warn + fail-open) deliberately does not break builds; an
// operator opts into enforcement per feed.
package osv

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fondaco-dev/fondaco/core/api"
)

// DenyCode is the machine-readable reason for OSV denials.
const DenyCode = "known-vulnerability"

const defaultAPIURL = "https://api.osv.dev/v1/query"

// verdictNamespace scopes this policy's rows in the shared verdict cache.
const verdictNamespace = "osv"

func init() {
	api.RegisterPolicy("osv", New)
}

// Policy queries OSV for vulnerability verdicts.
type Policy struct {
	enforce  bool
	failOpen bool
	cacheTTL time.Duration
	apiURL   string
	client   *http.Client
	deps     api.PolicyServices

	// local is a per-process cache in front of the database one: it only
	// caches derived data with a TTL, so replicas stay stateless.
	mu    sync.Mutex
	local map[string]localEntry
	now   func() time.Time
}

type localEntry struct {
	vulnerable bool
	ids        string
	expires    time.Time
}

// New builds the policy from YAML options.
func New(options map[string]any, deps api.PolicyServices) (api.Policy, error) {
	p := &Policy{
		deps:     deps,
		failOpen: true,
		cacheTTL: 24 * time.Hour,
		apiURL:   defaultAPIURL,
		client:   &http.Client{Timeout: 10 * time.Second},
		local:    make(map[string]localEntry),
		now:      time.Now,
	}
	switch v, _ := options["mode"].(string); v {
	case "", "warn":
	case "enforce":
		p.enforce = true
	default:
		return nil, fmt.Errorf("osv: mode must be \"warn\" or \"enforce\", got %q", v)
	}
	if v, ok := options["fail_open"]; ok {
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("osv: fail_open must be a boolean")
		}
		p.failOpen = b
	}
	if v, ok := options["cache_ttl"]; ok {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("osv: cache_ttl must be a duration string")
		}
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("osv: invalid cache_ttl %q", s)
		}
		p.cacheTTL = d
	}
	if v, ok := options["api_url"].(string); ok && v != "" {
		p.apiURL = v
	}
	return p, nil
}

// check returns the verdict for an artifact.
func (p *Policy) check(ctx context.Context, a api.Artifact) api.Decision {
	ecosystem := a.Meta(api.MetaEcosystem)
	if ecosystem == "" || a.Coord.Name == "" || a.Coord.Version == "" {
		return api.Allowed() // nothing to query with
	}
	log := p.log()

	vulnerable, ids, err := p.verdict(ctx, ecosystem, a.Coord.Name, a.Coord.Version)
	if err != nil {
		if p.failOpen {
			log.Warn("OSV unavailable, allowing (fail-open)",
				"coordinate", a.Coord.String(), "error", err)
			return api.Allowed()
		}
		log.Warn("OSV unavailable, denying (fail-closed)",
			"coordinate", a.Coord.String(), "error", err)
		return api.Denied(DenyCode, fmt.Sprintf("vulnerability data unavailable for %s", a.Coord))
	}
	if !vulnerable {
		return api.Allowed()
	}
	if !p.enforce {
		log.Warn("known vulnerability (warn mode, allowed)",
			"coordinate", a.Coord.String(), "advisories", ids)
		return api.Allowed()
	}
	return api.Denied(DenyCode, fmt.Sprintf("%s has known vulnerabilities: %s", a.Coord, ids))
}

// verdict consults the per-process cache, then the database cache, then OSV.
func (p *Policy) verdict(ctx context.Context, ecosystem, pkg, version string) (bool, string, error) {
	key := ecosystem + "\x00" + pkg + "\x00" + version

	p.mu.Lock()
	e, ok := p.local[key]
	p.mu.Unlock()
	if ok && p.now().Before(e.expires) {
		return e.vulnerable, e.ids, nil
	}

	cacheKey := ecosystem + "/" + pkg + "@" + version
	if p.deps != nil {
		value, checkedAt, found, err := p.deps.GetVerdict(ctx, verdictNamespace, cacheKey)
		if err == nil && found && p.now().Sub(checkedAt) < p.cacheTTL {
			vulnerable, ids := decodeVerdict(value)
			p.remember(key, vulnerable, ids)
			return vulnerable, ids, nil
		}
	}

	vulnerable, ids, err := p.query(ctx, ecosystem, pkg, version)
	if err != nil {
		return false, "", err
	}
	if p.deps != nil {
		if err := p.deps.PutVerdict(ctx, verdictNamespace, cacheKey, encodeVerdict(vulnerable, ids)); err != nil {
			p.log().Debug("caching OSV verdict failed", "error", err)
		}
	}
	p.remember(key, vulnerable, ids)
	return vulnerable, ids, nil
}

// encodeVerdict/decodeVerdict store a verdict as "vulnerable|<ids>" or
// "clean" in the generic verdict cache.
func encodeVerdict(vulnerable bool, ids string) string {
	if !vulnerable {
		return "clean"
	}
	return "vulnerable|" + ids
}

func decodeVerdict(value string) (bool, string) {
	rest, ok := strings.CutPrefix(value, "vulnerable|")
	if !ok {
		return false, ""
	}
	return true, rest
}

func (p *Policy) log() *slog.Logger {
	if p.deps != nil {
		if l := p.deps.Logger(); l != nil {
			return l
		}
	}
	return slog.Default()
}

func (p *Policy) remember(key string, vulnerable bool, ids string) {
	p.mu.Lock()
	p.local[key] = localEntry{vulnerable: vulnerable, ids: ids, expires: p.now().Add(p.cacheTTL)}
	p.mu.Unlock()
}

type osvRequest struct {
	Version string     `json:"version"`
	Package osvPackage `json:"package"`
}

type osvPackage struct {
	Name      string `json:"name"`
	Ecosystem string `json:"ecosystem"`
}

type osvResponse struct {
	Vulns []struct {
		ID        string `json:"id"`
		Withdrawn string `json:"withdrawn"`
	} `json:"vulns"`
}

// query asks OSV.dev about one package version.
func (p *Policy) query(ctx context.Context, ecosystem, pkg, version string) (bool, string, error) {
	payload, err := json.Marshal(osvRequest{
		Version: version,
		Package: osvPackage{Name: pkg, Ecosystem: ecosystem},
	})
	if err != nil {
		return false, "", fmt.Errorf("encode OSV query: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiURL, bytes.NewReader(payload))
	if err != nil {
		return false, "", fmt.Errorf("build OSV request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return false, "", fmt.Errorf("query OSV: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, "", fmt.Errorf("OSV returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return false, "", fmt.Errorf("read OSV response: %w", err)
	}
	var doc osvResponse
	if err := json.Unmarshal(body, &doc); err != nil {
		return false, "", fmt.Errorf("decode OSV response: %w", err)
	}
	ids := make([]string, 0, len(doc.Vulns))
	for _, v := range doc.Vulns {
		if v.Withdrawn != "" {
			continue // withdrawn advisories are not findings
		}
		ids = append(ids, v.ID)
	}
	sort.Strings(ids)
	return len(ids) > 0, strings.Join(ids, ","), nil
}

// OnResolve implements api.Policy: the coordinate alone is enough to query
// OSV, so vulnerable versions are stopped before any download happens.
func (p *Policy) OnResolve(ctx context.Context, _ api.Identity, c api.PackageCoordinate) api.Decision {
	if c.Version == "" {
		return api.Allowed()
	}
	return p.check(ctx, api.Artifact{Coord: c, Metadata: map[string]string{
		api.MetaEcosystem: ecosystemFor(c.Format),
	}})
}

// ecosystemFor maps a registry format onto an OSV ecosystem name. Format
// modules can override it through api.MetaEcosystem; this fallback only
// exists for OnResolve, where no artifact metadata has been fetched yet.
func ecosystemFor(format string) string {
	switch format {
	case "maven":
		return "Maven"
	case "npm":
		return "npm"
	case "nuget":
		return "NuGet"
	case "composer":
		return "Packagist"
	default:
		return ""
	}
}

// OnServe implements api.Policy.
func (p *Policy) OnServe(ctx context.Context, _ api.Identity, a api.Artifact) api.Decision {
	return p.check(ctx, a)
}

// OnPublish implements api.Policy.
func (p *Policy) OnPublish(ctx context.Context, _ api.Identity, a api.Artifact) api.Decision {
	return p.check(ctx, a)
}
