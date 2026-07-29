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
		w = withChallenge(w, fr.module, fr.feed)

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
		// The coordinate is not known until the module has parsed the
		// upload, so the feed-wide question is asked first and the
		// coordinate-level one inside CoreServices.Publish, where the
		// coordinate exists. Both go through the same engine.
		if !rt.mayPublishSomething(id, fr.feed.Name) {
			d := rt.mayPublish(id, fr.feed.Name, "")
			s.audit.Warn("publish denied",
				"feed", fr.feed.Name, "identity", id.String(),
				"project_path", id.ProjectPath, "path", r.URL.Path,
				"reason", d.Reason, "policies", strings.Join(d.Policies, ","))
			s.writeAccessError(w, id, d)
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
			runtime:      rt,
			feed:         fr,
			identity:     id,
		}
		// Provenance is set before the module can write anything, so a
		// module-authored response carries it too (invariant 11).
		w.Header().Set(api.SourceHeader, string(api.SourceLocal))
		w.Header().Set(api.SiteHeader, s.site)

		responder, writesOwnResponse := hoster.(api.PublishResponder)
		if writesOwnResponse {
			err = responder.HandlePublishHTTP(ctx, fr.feed, w, r, deps)
		} else {
			err = hoster.HandlePublish(ctx, fr.feed, r, deps)
		}
		if err != nil {
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

		if !writesOwnResponse {
			w.WriteHeader(http.StatusCreated)
		}
	}
}

// publishDeps decorates the core publisher with per-request policy checks:
// the module never sees identities or policies, it just publishes.
type publishDeps struct {
	api.CoreServices
	server   *Server
	runtime  *runtime
	feed     *feedRuntime
	identity api.Identity
}

// Publish enforces the feed's policy chain (OnPublish) before committing.
func (d *publishDeps) Publish(ctx context.Context, req api.PublishRequest) (api.PublishResult, error) {
	req.Identity = d.identity

	// The binding access check. Only here is the coordinate known, so only
	// here can "may publish com.example, and nothing else" be enforced —
	// the gate before the upload was read could not have known.
	if decision := d.runtime.mayPublish(d.identity, d.feed.feed.Name, req.Coord.String()); !decision.Allowed {
		d.server.audit.Warn("publish denied for this coordinate",
			"feed", d.feed.feed.Name, "identity", d.identity.String(),
			"coordinate", req.Coord.String(), "reason", decision.Reason)
		return api.PublishResult{}, fmt.Errorf("%s: %w", decision.Reason, api.ErrForbidden)
	}

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
	status, header, body, err := (*forward)(r.Context(), home, fr.feed.Name,
		strings.TrimPrefix(r.URL.Path, "/"), r.Method, r.Body, payloadHeaders(r.Header), id)
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
	// The home site's protocol headers are the answer, not decoration: an
	// upload session is continued from the Location it names.
	copyForwardedHeaders(w.Header(), header)
	w.Header().Set(api.SiteHeader, home)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// payloadHeaders are the request headers that describe WHAT is being
// published, as opposed to WHO is publishing it.
//
// They have to travel: a NuGet push is a multipart body and is unreadable
// without its Content-Type, and a chunked upload means nothing without its
// Content-Range. Nothing about the caller travels — the client's credential
// stays at the site that authenticated it, and the home site authorizes the
// on-behalf-of identity instead (invariant 14).
func payloadHeaders(src http.Header) http.Header {
	out := http.Header{}
	for _, name := range []string{
		"Content-Type", "Content-Range", "Content-Encoding", "Content-Disposition", "Accept",
	} {
		if v := src.Get(name); v != "" {
			out.Set(name, v)
		}
	}
	return out
}

// copyForwardedHeaders passes the home site's response headers on.
//
// Length and encoding describe the hop that just ended, not the one about to
// begin, and copying them would describe this response wrongly.
func copyForwardedHeaders(dst, src http.Header) {
	for name, values := range src {
		switch http.CanonicalHeaderKey(name) {
		case "Content-Length", "Transfer-Encoding", "Connection", "Date":
			continue
		}
		for _, v := range values {
			dst.Add(name, v)
		}
	}
}

// ApplyForwardedPublish handles a write another site accepted on our behalf
// because this site is the feed's home. The peer authenticated the client,
// but authorization is re-checked here against the on-behalf-of identity:
// replication grants no authority (invariant 14).
func (s *Server) ApplyForwardedPublish(ctx context.Context, feed, path, method string,
	body io.ReadCloser, header http.Header, identity, projectPath, peer string) (int, http.Header, []byte, error) {
	defer func() { _ = body.Close() }()

	rt := s.rt.Load()
	fr, ok := rt.feeds[feed]
	if !ok {
		return http.StatusNotFound, nil, []byte("unknown feed " + feed + "\n"), nil
	}
	if !fr.publish.Local {
		// We are not the home site either: refuse rather than chain
		// forwards, which could loop.
		return http.StatusMisdirectedRequest, nil,
			[]byte("this site is not the home of feed " + feed + "\n"), nil
	}
	hoster, ok := fr.module.(api.Hoster)
	if !ok || !fr.hosted {
		return http.StatusMethodNotAllowed, nil, []byte("feed " + feed + " does not accept publishes\n"), nil
	}
	if !s.publisher.Enabled() {
		return http.StatusServiceUnavailable, nil, []byte("publishing is unavailable: no database\n"), nil
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
		return http.StatusForbidden, nil, []byte("forwarded identity is not a token or OIDC subject\n"), nil
	}
	if !rt.mayPublishSomething(id, feed) {
		d := rt.mayPublish(id, feed, "")
		s.audit.Warn("forwarded publish denied",
			"feed", feed, "path", path, "identity", identity, "peer", peer,
			"reason", d.Reason)
		return http.StatusForbidden, nil, []byte(d.Reason + "\n"), nil
	}

	if method == "" {
		method = http.MethodPut
	}
	// Modules see feed-relative paths (the mount prefix is stripped in the
	// normal flow), so the forwarded request must look identical.
	req, err := http.NewRequestWithContext(ctx, method, "/"+strings.TrimPrefix(path, "/"), body)
	if err != nil {
		return 0, nil, nil, err
	}
	// The headers that describe the payload came with it; without them a
	// multipart upload is stored as its own envelope.
	for name, values := range payloadHeaders(header) {
		req.Header[name] = values
	}

	deps := &publishDeps{
		CoreServices: s.publisher,
		server:       s,
		runtime:      rt,
		feed:         fr,
		identity:     id,
	}
	// A module that writes its own response writes it into a recorder, so
	// the forwarding site can hand the client exactly what the home site
	// answered — headers included, because for some protocols they are the
	// answer.
	responder, writesOwnResponse := hoster.(api.PublishResponder)
	rec := httptest.NewRecorder()
	if writesOwnResponse {
		err = responder.HandlePublishHTTP(ctx, fr.feed, rec, req, deps)
	} else {
		err = hoster.HandlePublish(ctx, fr.feed, req, deps)
	}
	if err != nil {
		failed := httptest.NewRecorder()
		s.writeErrorText(failed, err, err.Error())
		s.audit.Warn("forwarded publish rejected",
			"feed", feed, "path", path, "identity", identity, "peer", peer,
			"status", failed.Code, "error", err)
		return failed.Code, failed.Header(), failed.Body.Bytes(), nil
	}
	if err := s.publisher.Reindex(ctx, fr.feed, fr.module); err != nil {
		s.logger.Error("reindex after forwarded publish failed", "feed", feed, "error", err)
	}
	s.audit.Info("forwarded publish accepted",
		"feed", feed, "path", path, "identity", identity, "forwarded_by", peer, "site", s.site)
	if writesOwnResponse {
		return rec.Code, rec.Header(), rec.Body.Bytes(), nil
	}
	return http.StatusCreated, nil, []byte("published\n"), nil
}
