package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/sasokolov/package-registry/core/api"
)

// publishRoutes returns the write routes a hosting module serves. Modules
// may declare them explicitly via api.PublishRouter; otherwise the catch-all
// PUT/POST pair is used, which fits every protocol implemented so far.
func publishRoutes(module api.FormatModule) []api.Route {
	if pr, ok := module.(api.PublishRouter); ok {
		return pr.PublishRoutes()
	}
	return []api.Route{
		{Method: http.MethodPut, Pattern: "/*"},
		{Method: http.MethodPost, Pattern: "/*"},
	}
}

// publishHandler runs the write chain: auth → publish permission → module
// (which stages blobs and commits through CoreServices.Publish) → reindex.
func (s *Server) publishHandler(rt *runtime, fr *feedRuntime, hoster api.Hoster) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		id, err := rt.authn.Identify(ctx, r)
		if err != nil {
			s.audit.Warn("publish authentication rejected",
				"feed", fr.feed.Name, "remote", r.RemoteAddr, "error", err)
			s.writeError(w, err, "")
			return
		}
		if id.IsAnonymous() {
			s.writeError(w, api.ErrUnauthorized, "authentication required to publish")
			return
		}
		if !fr.publishers.Allowed(id) {
			s.audit.Warn("publish denied: identity has no publish permission",
				"feed", fr.feed.Name, "identity", id.String(),
				"project_path", id.ProjectPath, "path", r.URL.Path,
				"allowed", fr.publishers.Describe())
			s.writeError(w, api.ErrForbidden, "identity may not publish to this feed")
			return
		}
		// Write-affinity: a feed homed at another site is published there,
		// so the immutability check (409) stays authoritative
		// (docs/geo-replication.md).
		if !fr.publish.Local {
			s.forwardPublish(w, r, fr, id)
			return
		}
		if !s.publisher.Enabled() {
			s.writeError(w, api.ErrUnavailable, "publishing is unavailable: no database")
			return
		}

		deps := &publishDeps{
			CoreServices: s.publisher,
			server:       s,
			feed:         fr,
			identity:     id,
		}
		if err := hoster.HandlePublish(ctx, fr.feed, r, deps); err != nil {
			s.logger.Warn("publish failed",
				"feed", fr.feed.Name, "path", r.URL.Path, "identity", id.String(), "error", err)
			s.writeErrorText(w, err, err.Error())
			return
		}

		// Feed indexes are derived data: rebuild them locally from the
		// converged manifest set (invariant 15).
		if err := s.publisher.Reindex(ctx, fr.feed, fr.module); err != nil {
			s.logger.Error("reindex after publish failed",
				"feed", fr.feed.Name, "error", err)
		}

		w.Header().Set(api.SourceHeader, string(api.SourceLocal))
		w.Header().Set("X-Registry-Site", s.site)
		w.WriteHeader(http.StatusCreated)
	}
}

// publishDeps decorates the core publisher with per-request policy checks:
// the module never sees identities or policies, it just publishes.
type publishDeps struct {
	api.CoreServices
	server   *Server
	feed     *feedRuntime
	identity api.Identity
}

// Publish enforces the feed's policy chain (OnPublish) before committing.
func (d *publishDeps) Publish(ctx context.Context, req api.PublishRequest) (api.PublishResult, error) {
	req.Identity = d.identity
	artifact := api.Artifact{
		Coord:    req.Coord,
		Size:     req.Size,
		Checksum: api.Checksum{Algo: "sha256", Hex: req.SHA256},
		Metadata: nonNil(req.Metadata),
	}
	if decision := d.feed.chain.OnPublish(ctx, d.identity, artifact); !decision.Allow {
		d.server.auditDeny("publish", d.feed, d.identity, req.Coord, decision)
		return api.PublishResult{}, publishDenied(decision)
	}
	return d.CoreServices.Publish(ctx, req)
}

func publishDenied(d api.Decision) error {
	return fmt.Errorf("%s: %w", d.Reason, api.ErrForbidden)
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// forwardPublish proxies a publish to the feed's home site. The client is
// authenticated HERE and the forwarded request carries an on-behalf-of
// identity, so the home site audits the real publisher. Never a redirect:
// package clients drop credentials across hosts.
func (s *Server) forwardPublish(w http.ResponseWriter, r *http.Request, fr *feedRuntime, id api.Identity) {
	home := fr.publish.HomeSite
	if s.forward == nil {
		s.audit.Warn("publish to a remotely-homed feed refused: forwarding is not configured",
			"feed", fr.feed.Name, "home_site", home, "identity", id.String())
		w.Header().Set("Retry-After", "30")
		w.Header().Set("X-Registry-Home-Site", home)
		s.finishError(w, http.StatusServiceUnavailable,
			fmt.Sprintf("feed %s is published at site %s; this site cannot forward writes", fr.feed.Name, home))
		return
	}
	status, body, err := s.forward(r.Context(), home, r, id)
	if err != nil {
		s.audit.Warn("publish forwarding failed",
			"feed", fr.feed.Name, "home_site", home, "identity", id.String(), "error", err)
		w.Header().Set("Retry-After", "30")
		w.Header().Set("X-Registry-Home-Site", home)
		s.finishError(w, http.StatusServiceUnavailable,
			fmt.Sprintf("home site %s for feed %s is unreachable: %v", home, fr.feed.Name, err))
		return
	}
	s.audit.Info("publish forwarded to home site",
		"feed", fr.feed.Name, "home_site", home, "identity", id.String(), "status", status)
	w.Header().Set(api.SiteHeader, home)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
