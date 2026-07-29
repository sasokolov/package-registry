package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/fondaco-dev/fondaco/core/api"
	"github.com/fondaco-dev/fondaco/core/pipeline"
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
		w = withChallenge(w, fr.module, fr.feed)

		adoptDeclaredCredential(fr.module, r)
		id, err := rt.authn.Identify(ctx, r)
		if err != nil {
			s.audit.Warn("authentication rejected",
				"feed", fr.feed.Name, "remote", r.RemoteAddr, "error", err)
			s.writeError(w, err, "")
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

		// Access is decided on the coordinate, not on the feed: a rule can
		// grant a namespace inside a feed and deny the rest.
		if d := rt.mayServe(id, fr.feed.Name, intent); !d.Allowed {
			s.audit.Warn("access denied",
				"feed", fr.feed.Name, "identity", id.String(),
				"coordinate", intent.Coord.String(), "reason", d.Reason,
				"policies", strings.Join(d.Policies, ","))
			s.writeAccessError(w, id, d)
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
		if s.serveSearch(ctx, w, fr, intent) {
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
			s.served(fr.feed.Name, intent, res)
			return
		}

		w.Header().Set(api.SourceHeader, string(res.Source))
		w.Header().Set(api.SiteHeader, s.site)
		if res.Size >= 0 {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", res.Size))
		}
		w.Header().Set("Content-Type", responseContentType(intent, res))
		setProtocolHeaders(w, fr.module, fr.feed, intent, res.SHA256)
		if _, err := io.Copy(w, res.Body); err != nil {
			// Response already started; nothing to send, just record it.
			s.logger.Debug("client aborted download",
				"feed", fr.feed.Name, "coord", intent.Coord.String(), "error", err)
		}
		s.served(fr.feed.Name, intent, res)
	}
}

// responseContentType decides what a body is served as.
//
// The module's own answer wins: it knows what a path means. Only where it
// declined to say — because the media type is a property of the content and
// not of the path — does the type recorded at ingest stand in.
func responseContentType(intent api.Intent, res *pipeline.Result) string {
	if intent.ContentType != "" {
		return intent.ContentType
	}
	if res != nil && res.ContentType != "" {
		return res.ContentType
	}
	return "application/octet-stream"
}

// setProtocolHeaders adds the response headers a protocol requires and whose
// names only its module knows (api.ResponseHeaderer).
func setProtocolHeaders(w http.ResponseWriter, module api.FormatModule, feed api.Feed,
	intent api.Intent, sha256hex string) {
	headerer, ok := module.(api.ResponseHeaderer)
	if !ok {
		return
	}
	for name, value := range headerer.ResponseHeaders(feed, intent, sha256hex) {
		if value != "" {
			w.Header().Set(name, value)
		}
	}
}

// challengeWriter replaces the registry's default WWW-Authenticate with the
// one this protocol's clients can act on (api.AuthChallenger).
//
// It is a decorator rather than a header set up front because a challenge
// belongs on a 401 and nowhere else: advertising on every response how to
// authenticate would invite clients to do it unprompted.
type challengeWriter struct {
	http.ResponseWriter
	challenge string
}

func (c *challengeWriter) WriteHeader(status int) {
	if status == http.StatusUnauthorized {
		c.Header().Set("WWW-Authenticate", c.challenge)
	}
	c.ResponseWriter.WriteHeader(status)
}

// withChallenge wraps w when the module declares its own challenge.
func withChallenge(w http.ResponseWriter, module api.FormatModule, feed api.Feed) http.ResponseWriter {
	challenger, ok := module.(api.AuthChallenger)
	if !ok {
		return w
	}
	challenge := challenger.AuthChallenge(feed)
	if challenge == "" {
		return w
	}
	return &challengeWriter{ResponseWriter: w, challenge: challenge}
}

// served records one delivered response.
//
// It is here rather than in the pipeline because only this layer knows a
// request came from a client: the pipeline is also asked for the sibling
// documents a policy needs, and counting those as downloads would make every
// Maven artifact look like two.
//
// A redirected artifact counts too. The bytes leave the object store instead
// of this process, but the question an operator is asking — how much is this
// feed used — has the same answer either way.
func (s *Server) served(feed string, intent api.Intent, res *pipeline.Result) {
	if s.usage == nil || res == nil {
		return
	}
	s.usage.Served(feed, string(res.Source), downloadedCoordinate(intent), res.Size)
}

// downloadedCoordinate is what a "most downloaded" list should name.
//
// Only artifacts count: a metadata document is fetched on every resolve, so
// listing those would produce a leaderboard of whatever people happened to
// look up rather than of what they installed. Checksum sidecars are the same
// request as the artifact they belong to, and counting them would double
// every Maven download.
func downloadedCoordinate(intent api.Intent) string {
	if intent.Kind != api.IntentArtifact || intent.WantChecksum != "" {
		return ""
	}
	return intent.Coord.String()
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

// serveSearch answers a search from what the feed hosts, and reports whether
// it did.
//
// Only a hosting feed answers its own searches: a proxy has an upstream that
// implements search properly, and a registry inventing a ranking over a
// cache it happens to hold would be worse than passing the question on. A
// feed that both hosts and proxies asks its upstream too — the local answer
// alone would silently hide everything the upstream knows.
func (s *Server) serveSearch(ctx context.Context, w http.ResponseWriter,
	fr *feedRuntime, intent api.Intent) bool {
	if intent.Kind != api.IntentSearch || !fr.hosted || fr.upstream != nil {
		return false
	}
	searcher, ok := fr.module.(api.Searcher)
	if !ok {
		return false
	}
	res, err := searcher.Search(ctx, fr.feed, intent, s.publisher)
	if err != nil {
		s.writeError(w, err, "")
		return true
	}
	writeSynthetic(w, s.site, res)
	return true
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
	writeSynthetic(w, s.site, resp)
}

// writeSynthetic emits a response the registry produced itself.
func writeSynthetic(w http.ResponseWriter, site string, resp api.SyntheticResponse) {
	for k, v := range resp.Header {
		w.Header().Set(k, v)
	}
	w.Header().Set(api.SourceHeader, string(api.SourceLocal))
	w.Header().Set(api.SiteHeader, site)
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
		w.Header().Set("WWW-Authenticate", `Basic realm="fondaco", Bearer realm="fondaco"`)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg+"\n")
}
