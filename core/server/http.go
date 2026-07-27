package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/sasokolov/package-registry/core/api"
	"github.com/sasokolov/package-registry/core/pipeline"
)

// adoptDeclaredCredential lets a protocol that carries its credential
// outside the Authorization header still authenticate.
//
// The header names come from the module, because the core has no business
// knowing that NuGet calls its credential an API key. An Authorization
// header that is already present always wins; what is adopted is verified
// exactly like any other bearer credential, so this widens where a token
// may be written, never what it may do.
func adoptDeclaredCredential(module api.FormatModule, r *http.Request) {
	if r.Header.Get("Authorization") != "" {
		return
	}
	carrier, ok := module.(api.CredentialHeader)
	if !ok {
		return
	}
	for _, name := range carrier.CredentialHeaders() {
		if value := strings.TrimSpace(r.Header.Get(name)); value != "" {
			r.Header.Set("Authorization", "Bearer "+value)
			return
		}
	}
}

// feedHandler runs the full chain for one feed: auth → policy(OnResolve) →
// pipeline → policy(OnServe) → stream, with the source header on success.
func (s *Server) feedHandler(rt *runtime, fr *feedRuntime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		adoptDeclaredCredential(fr.module, r)
		id, err := rt.authn.Identify(ctx, r)
		if err != nil {
			s.audit.Warn("authentication rejected",
				"feed", fr.feed.Name, "remote", r.RemoteAddr, "error", err)
			s.writeError(w, err, "")
			return
		}
		if id.IsAnonymous() && !fr.feed.Anonymous {
			s.audit.Warn("anonymous access denied",
				"feed", fr.feed.Name, "path", r.URL.Path, "remote", r.RemoteAddr)
			s.writeError(w, api.ErrUnauthorized, "authentication required")
			return
		}

		intent, err := fr.module.Parse(r)
		if err != nil {
			// Parse errors are module-authored and client-safe by contract:
			// they explain protocol-level rejections (e.g. Maven SNAPSHOTs
			// not being proxied yet).
			s.writeErrorText(w, err, err.Error())
			return
		}

		// Quarantined coordinates are never served, whatever the policies
		// say (manual takedown, cross-site publish conflict).
		if blocked, reason := s.quarantine.Blocked(ctx, fr.feed.Name, intent.Coord.String()); blocked {
			if strings.HasPrefix(reason, "cross_site_conflict") {
				// Invariant 11: the header carries the CONFLICTED
				// COORDINATE, so a client (and a log line) names what to
				// look up with `repl conflicts`. Other quarantine reasons
				// are not conflicts and do not set it.
				w.Header().Set("X-Registry-Conflict", intent.Coord.String())
			}
			s.audit.Warn("quarantined coordinate requested",
				"feed", fr.feed.Name, "identity", id.String(),
				"coordinate", intent.Coord.String(), "reason", reason)
			s.writeError(w, api.ErrQuarantined, reason)
			return
		}

		if d := fr.chain.OnResolve(ctx, id, intent.Coord); !d.Allow {
			s.auditDeny("resolve", fr, id, intent.Coord, d)
			s.writeError(w, api.ErrForbidden, d.Reason)
			return
		}

		if intent.Kind == api.IntentSynthetic {
			s.serveSynthetic(w, fr, intent)
			return
		}

		res, err := s.pipe.Serve(ctx, pipeline.Request{
			Feed:         fr.feed,
			Intent:       intent,
			Module:       fr.module,
			Upstream:     fr.upstream,
			PeerFallback: fr.peerFallback,
		})
		s.updateBreakerGauge(fr)
		if err != nil {
			s.logger.Warn("pipeline error",
				"feed", fr.feed.Name, "coord", intent.Coord.String(), "error", err)
			s.writeError(w, err, "")
			return
		}
		defer func() { _ = res.Body.Close() }()

		artifact := api.Artifact{
			Coord:    intent.Coord,
			Size:     res.Size,
			Checksum: api.Checksum{Algo: "sha256", Hex: res.SHA256},
			Metadata: s.artifactMetadata(ctx, fr, intent),
		}
		if d := fr.chain.OnServe(ctx, id, artifact); !d.Allow {
			s.auditDeny("serve", fr, id, intent.Coord, d)
			s.writeError(w, api.ErrForbidden, d.Reason)
			return
		}

		if s.tryRedirect(ctx, w, r, fr, intent, res) {
			return
		}

		w.Header().Set(api.SourceHeader, string(res.Source))
		w.Header().Set(api.SiteHeader, s.site)
		if res.Size >= 0 {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", res.Size))
		}
		contentType := intent.ContentType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		w.Header().Set("Content-Type", contentType)
		if _, err := io.Copy(w, res.Body); err != nil {
			// Response already started; nothing to send, just record it.
			s.logger.Debug("client aborted download",
				"feed", fr.feed.Name, "coord", intent.Coord.String(), "error", err)
		}
	}
}

// tryRedirect answers a cached artifact with a 302 to a pre-signed storage
// URL when the feed opted in, the storage can pre-sign and the protocol is
// redirect-safe. Any failure falls back to streaming: a redirect is an
// optimization, never a correctness requirement.
func (s *Server) tryRedirect(ctx context.Context, w http.ResponseWriter, r *http.Request,
	fr *feedRuntime, intent api.Intent, res *pipeline.Result) bool {
	if !fr.redirect || res.BlobKey == "" || r.Method != http.MethodGet {
		return false
	}
	safe, ok := fr.module.(api.RedirectSafe)
	if !ok || !safe.RedirectSafeIntent(intent) {
		return false
	}
	presigner, ok := s.store.(api.Presigner)
	if !ok {
		return false
	}
	url, err := presigner.PresignGet(ctx, res.BlobKey, fr.redirectTTL)
	if err != nil {
		s.logger.Warn("pre-signing failed, streaming instead",
			"feed", fr.feed.Name, "coord", intent.Coord.String(), "error", err)
		return false
	}
	w.Header().Set(api.SourceHeader, string(res.Source))
	w.Header().Set("X-Registry-Site", s.site)
	http.Redirect(w, r, url, http.StatusFound)
	return true
}

// artifactMetadata asks the format module which document carries the
// artifact's metadata (Maven: the sibling .pom), fetches it through the
// normal cached pipeline and lets the module translate it into canonical
// keys. Policies therefore see licenses and publication dates without any
// format knowledge (invariant 1). Failures degrade to empty metadata: a
// policy that needs a key decides for itself what missing means.
func (s *Server) artifactMetadata(ctx context.Context, fr *feedRuntime, intent api.Intent) map[string]string {
	if intent.Kind != api.IntentArtifact || intent.WantChecksum != "" {
		return map[string]string{}
	}
	source, ok := fr.module.(api.MetadataSource)
	if !ok {
		return map[string]string{}
	}
	metaIntent, ok := source.MetadataIntent(fr.feed, intent.Coord)
	if !ok {
		return map[string]string{}
	}
	res, err := s.pipe.Serve(ctx, pipeline.Request{
		Feed: fr.feed, Intent: metaIntent, Module: fr.module,
		Upstream: fr.upstream, PeerFallback: fr.peerFallback,
	})
	if err != nil {
		s.logger.Debug("artifact metadata unavailable",
			"feed", fr.feed.Name, "coord", intent.Coord.String(), "error", err)
		return map[string]string{}
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return map[string]string{}
	}
	meta, err := source.ExtractMetadata(intent.Coord, body)
	if err != nil {
		s.logger.Debug("artifact metadata unparsable",
			"feed", fr.feed.Name, "coord", intent.Coord.String(), "error", err)
		return map[string]string{}
	}
	if meta == nil {
		return map[string]string{}
	}
	return meta
}

func (s *Server) auditDeny(action string, fr *feedRuntime, id api.Identity, coord api.PackageCoordinate, d api.Decision) {
	s.audit.Warn("policy denied",
		"action", action,
		"feed", fr.feed.Name,
		"identity", id.String(),
		"project_path", id.ProjectPath,
		"coordinate", coord.String(),
		"policy", d.Policy,
		"code", d.Code,
		"reason", d.Reason,
	)
}

func (s *Server) updateBreakerGauge(fr *feedRuntime) {
	if s.metrics != nil && fr.upstream != nil {
		s.metrics.BreakerState.WithLabelValues(fr.feed.Name).Set(float64(fr.upstream.BreakerState()))
	}
}

// serveSynthetic answers protocol-level endpoints from the module alone
// (no cache, no upstream); labeled X-Registry-Source: local.
func (s *Server) serveSynthetic(w http.ResponseWriter, fr *feedRuntime, intent api.Intent) {
	syn, ok := fr.module.(api.Synthesizer)
	if !ok {
		s.logger.Error("module returned a synthetic intent without the Synthesizer capability",
			"format", fr.feed.Format)
		s.writeError(w, errors.New("module misconfigured"), "")
		return
	}
	resp, err := syn.Synthesize(fr.feed, intent)
	if err != nil {
		s.writeErrorText(w, err, err.Error())
		return
	}
	for k, v := range resp.Header {
		w.Header().Set(k, v)
	}
	w.Header().Set(api.SourceHeader, string(api.SourceLocal))
	w.Header().Set(api.SiteHeader, s.site)
	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if len(resp.Body) > 0 {
		_, _ = w.Write(resp.Body)
	}
}

// errorStatus maps sentinel errors to an HTTP status and a generic message.
func errorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, api.ErrNotFound):
		return http.StatusNotFound, "not found"
	case errors.Is(err, api.ErrUnauthorized):
		return http.StatusUnauthorized, "unauthorized"
	case errors.Is(err, api.ErrForbidden):
		return http.StatusForbidden, "forbidden"
	case errors.Is(err, api.ErrChecksumMismatch):
		return http.StatusBadGateway, "upstream artifact failed checksum verification"
	case errors.Is(err, api.ErrUpstreamUnavailable):
		return http.StatusBadGateway, "upstream unavailable and no cached copy"
	case errors.Is(err, api.ErrUnavailable):
		return http.StatusServiceUnavailable, "temporarily unavailable"
	case errors.Is(err, api.ErrImmutable):
		return http.StatusConflict, "published release is immutable"
	case errors.Is(err, api.ErrQuarantined):
		return http.StatusConflict, "coordinate is quarantined"
	case errors.Is(err, api.ErrBadRequest):
		return http.StatusBadRequest, "bad request"
	default:
		return http.StatusInternalServerError, "internal error"
	}
}

// writeError responds with the generic message for err; detail, when
// non-empty, is appended (e.g. a policy reason).
func (s *Server) writeError(w http.ResponseWriter, err error, detail string) {
	status, msg := errorStatus(err)
	if detail != "" {
		msg = msg + ": " + detail
	}
	s.finishError(w, status, msg)
}

// writeErrorText responds with a fully client-authored message (module
// Parse/Synthesize errors are client-safe by contract).
func (s *Server) writeErrorText(w http.ResponseWriter, err error, text string) {
	status, msg := errorStatus(err)
	if text != "" {
		msg = text
	}
	s.finishError(w, status, msg)
}

func (s *Server) finishError(w http.ResponseWriter, status int, msg string) {
	// Every response carries its provenance, error or not (invariant 11):
	// a 409 from a federation conflict has to say which site answered, and
	// that is exactly when an operator reads the header.
	if w.Header().Get(api.SourceHeader) == "" {
		w.Header().Set(api.SourceHeader, string(api.SourceLocal))
	}
	if w.Header().Get(api.SiteHeader) == "" {
		w.Header().Set(api.SiteHeader, s.site)
	}
	if status == http.StatusUnauthorized {
		// Basic is advertised too: maven-resolver and Gradle send
		// username/password credentials only for a scheme they support.
		w.Header().Set("WWW-Authenticate", `Basic realm="package-registry", Bearer realm="package-registry"`)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg+"\n")
}
