package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

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

		adoptDeclaredCredential(fr.module, r)
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
		if id.Stale {
			s.audit.Warn("publish refused: identity could not be re-verified",
				"feed", fr.feed.Name, "identity", id.String(), "path", r.URL.Path)
			s.writeError(w, api.ErrUnavailable,
				"the token backend is unavailable, so this credential cannot be verified for a write")
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
	forward := s.forward.Load()
	if forward == nil {
		s.audit.Warn("publish to a remotely-homed feed refused: forwarding is not configured",
			"feed", fr.feed.Name, "home_site", home, "identity", id.String())
		w.Header().Set("Retry-After", "30")
		w.Header().Set("X-Registry-Home-Site", home)
		s.finishError(w, http.StatusServiceUnavailable,
			fmt.Sprintf("feed %s is published at site %s; this site cannot forward writes", fr.feed.Name, home))
		return
	}
	// Inside a feed handler the mount prefix is stripped; the home site
	// addresses the coordinate by feed name and path.
	status, body, err := (*forward)(r.Context(), home, fr.feed.Name,
		strings.TrimPrefix(r.URL.Path, "/"), r.Method, r.Body, id)
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

// ApplyForwardedPublish handles a write another site accepted on our behalf
// because this site is the feed's home. The peer authenticated the client,
// but authorization is re-checked here against the on-behalf-of identity:
// replication grants no authority (invariant 14).
func (s *Server) ApplyForwardedPublish(ctx context.Context, feed, path, method string,
	body io.ReadCloser, identity, projectPath, peer string) (int, []byte, error) {
	defer func() { _ = body.Close() }()

	rt := s.rt.Load()
	fr, ok := rt.feeds[feed]
	if !ok {
		return http.StatusNotFound, []byte("unknown feed " + feed + "\n"), nil
	}
	if !fr.publish.Local {
		// We are not the home site either: refuse rather than chain
		// forwards, which could loop.
		return http.StatusMisdirectedRequest,
			[]byte("this site is not the home of feed " + feed + "\n"), nil
	}
	hoster, ok := fr.module.(api.Hoster)
	if !ok || !fr.hosted {
		return http.StatusMethodNotAllowed, []byte("feed " + feed + " does not accept publishes\n"), nil
	}
	if !s.publisher.Enabled() {
		return http.StatusServiceUnavailable, []byte("publishing is unavailable: no database\n"), nil
	}

	// The peer authenticated the client and vouches for this identity. That
	// is a real trust delegation: a compromised peer could assert any
	// identity, so the mesh credential is what actually gates this path,
	// and the audit records the forwarding peer alongside the claimed
	// publisher. Authorization is still re-checked here — replication and
	// forwarding never widen what an identity may do (invariant 14).
	id := api.ParseIdentity(identity)
	id.ProjectPath = projectPath
	if id.Kind == api.IdentityAnonymous {
		s.audit.Warn("forwarded publish refused: unusable identity assertion",
			"feed", feed, "path", path, "identity", identity, "forwarded_by", peer)
		return http.StatusForbidden, []byte("forwarded identity is not a token or OIDC subject\n"), nil
	}
	if !fr.publishers.Allowed(id) {
		s.audit.Warn("forwarded publish denied: identity has no publish permission",
			"feed", feed, "path", path, "identity", identity, "peer", peer,
			"allowed", fr.publishers.Describe())
		return http.StatusForbidden, []byte("identity may not publish to this feed\n"), nil
	}

	if method == "" {
		method = http.MethodPut
	}
	// Modules see feed-relative paths (the mount prefix is stripped in the
	// normal flow), so the forwarded request must look identical.
	req, err := http.NewRequestWithContext(ctx, method, "/"+strings.TrimPrefix(path, "/"), body)
	if err != nil {
		return 0, nil, err
	}

	deps := &publishDeps{
		CoreServices: s.publisher,
		server:       s,
		feed:         fr,
		identity:     id,
	}
	if err := hoster.HandlePublish(ctx, fr.feed, req, deps); err != nil {
		rec := httptest.NewRecorder()
		s.writeErrorText(rec, err, err.Error())
		s.audit.Warn("forwarded publish rejected",
			"feed", feed, "path", path, "identity", identity, "peer", peer,
			"status", rec.Code, "error", err)
		return rec.Code, rec.Body.Bytes(), nil
	}
	if err := s.publisher.Reindex(ctx, fr.feed, fr.module); err != nil {
		s.logger.Error("reindex after forwarded publish failed", "feed", feed, "error", err)
	}
	s.audit.Info("forwarded publish accepted",
		"feed", feed, "path", path, "identity", identity, "forwarded_by", peer, "site", s.site)
	return http.StatusCreated, []byte("published\n"), nil
}
