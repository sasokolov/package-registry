package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/sasokolov/package-registry/core/api"
	"github.com/sasokolov/package-registry/core/pipeline"
)

// feedHandler runs the full chain for one feed: auth → policy(OnResolve) →
// pipeline → policy(OnServe) → stream, with the source header on success.
func (s *Server) feedHandler(rt *runtime, fr *feedRuntime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

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
			s.audit.Warn("quarantined coordinate requested",
				"feed", fr.feed.Name, "identity", id.String(),
				"coordinate", intent.Coord.String(), "reason", reason)
			w.Header().Set("X-Registry-Conflict", reason)
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
			Feed:     fr.feed,
			Intent:   intent,
			Module:   fr.module,
			Upstream: fr.upstream,
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

		w.Header().Set(api.SourceHeader, string(res.Source))
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
		Feed: fr.feed, Intent: metaIntent, Module: fr.module, Upstream: fr.upstream,
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
	if status == http.StatusUnauthorized {
		// Basic is advertised too: maven-resolver and Gradle send
		// username/password credentials only for a scheme they support.
		w.Header().Set("WWW-Authenticate", `Basic realm="package-registry", Bearer realm="package-registry"`)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg+"\n")
}
