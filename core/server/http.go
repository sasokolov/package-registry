package server

import (
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
			s.writeError(w, err, "")
			return
		}

		if d := fr.chain.OnResolve(ctx, id, intent.Coord); !d.Allow {
			s.auditDeny("resolve", fr, id, intent.Coord, d)
			s.writeError(w, api.ErrForbidden, d.Reason)
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
		w.Header().Set("Content-Type", "application/octet-stream")
		if _, err := io.Copy(w, res.Body); err != nil {
			// Response already started; nothing to send, just record it.
			s.logger.Debug("client aborted download",
				"feed", fr.feed.Name, "coord", intent.Coord.String(), "error", err)
		}
	}
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

// writeError maps sentinel errors to HTTP statuses. detail, when non-empty,
// is a client-safe explanation (e.g. a policy reason).
func (s *Server) writeError(w http.ResponseWriter, err error, detail string) {
	status := http.StatusInternalServerError
	msg := "internal error"
	switch {
	case errors.Is(err, api.ErrNotFound):
		status, msg = http.StatusNotFound, "not found"
	case errors.Is(err, api.ErrUnauthorized):
		status, msg = http.StatusUnauthorized, "unauthorized"
		w.Header().Set("WWW-Authenticate", `Bearer realm="package-registry"`)
	case errors.Is(err, api.ErrForbidden):
		status, msg = http.StatusForbidden, "forbidden"
	case errors.Is(err, api.ErrChecksumMismatch):
		status, msg = http.StatusBadGateway, "upstream artifact failed checksum verification"
	case errors.Is(err, api.ErrUpstreamUnavailable):
		status, msg = http.StatusBadGateway, "upstream unavailable and no cached copy"
	case errors.Is(err, api.ErrUnavailable):
		status, msg = http.StatusServiceUnavailable, "temporarily unavailable"
	case errors.Is(err, api.ErrImmutable):
		status, msg = http.StatusConflict, "published release is immutable"
	}
	if detail != "" {
		msg = msg + ": " + detail
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg+"\n")
}
