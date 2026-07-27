// Package adminapi serves the registry's own API: the read surface the web
// console needs, and the write surface that makes configuration manageable
// as code (Terraform) instead of only as a file somebody edits by hand.
//
// Configuration keeps the property invariant 8 exists for: there is exactly
// one declarative YAML document, it lives outside the database, and it is
// always replaced whole. What changes here is that the document has an API
// write path — validated before it is stored, serialized across replicas by
// an advisory lock, and guarded by an ETag so two writers cannot silently
// overwrite each other.
package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"gopkg.in/yaml.v3"

	"github.com/sasokolov/package-registry/core/access"
	"github.com/sasokolov/package-registry/core/api"
	"github.com/sasokolov/package-registry/core/config"
	"github.com/sasokolov/package-registry/core/repl"
	"github.com/sasokolov/package-registry/core/state"
)

// configLockKey serializes configuration writes across replicas. A write is
// a read-modify-write of one document, so two replicas doing it at once
// would lose one of the edits however careful each is on its own.
const configLockKey = "config-write"

// maxDocument bounds an uploaded document.
const maxDocument = 4 << 20

// Server implements the registry API.
type Server struct {
	manager *config.Manager
	db      *state.DB
	store   api.BlobStore
	logger  *slog.Logger
	audit   *slog.Logger
	site    string
	// deps supplies everything the read endpoints need without this
	// package importing the HTTP server.
	deps Deps
}

// Deps is what the API needs from the rest of the process. Passing them in
// keeps this package free of import cycles with core/server.
type Deps struct {
	// Identify resolves a request's credentials, exactly as the feed
	// routes do.
	Identify func(r *http.Request) (api.Identity, error)
	// FeedSummaries reports the configured feeds and what is known about
	// them (counts, upstream health).
	FeedSummaries func(ctx context.Context) []FeedSummary
	// Reindex rebuilds a feed's generated indexes after configuration or
	// content changed.
	Reindex func(ctx context.Context, feed string) error
	// CanPublish lists the feeds an identity may publish to, so the console
	// can hide actions instead of offering ones that will be refused.
	CanPublish func(id api.Identity) []string
	// Access returns the current access engine. It is a function because a
	// configuration reload replaces the engine, and a captured pointer
	// would keep answering with the rules that were in force at startup.
	Access func() *access.Engine
	// Projection writes the blob-store view of a coordinate, needed when
	// an operator resolves a conflict.
	Projection repl.ProjectionWriter
}

// Options wires the API server.
type Options struct {
	Manager *config.Manager
	DB      *state.DB
	Store   api.BlobStore
	Logger  *slog.Logger
	Audit   *slog.Logger
	Site    string
	Deps    Deps
}

// New builds the API server.
func New(o Options) *Server {
	s := &Server{
		manager: o.Manager, db: o.DB, store: o.Store,
		logger: o.Logger, audit: o.Audit, site: o.Site, deps: o.Deps,
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	if s.audit == nil {
		s.audit = s.logger
	}
	return s
}

// projector returns the projection writer for operator actions.
func (s *Server) projector() repl.ProjectionWriter { return s.deps.Projection }

// ---------------------------------------------------------------------------
// Authorization

// identity resolves the caller.
func (s *Server) identity(r *http.Request) api.Identity {
	id, _ := s.identityOrRefusal(r)
	return id
}

// identityOrRefusal resolves the caller and, when a credential was offered
// and refused, says so.
//
// The distinction matters more than it looks. Reporting a rejected token as
// plain anonymity means a client that pasted a truncated credential is told
// nothing at all — it simply keeps browsing as a stranger and wonders why
// nothing it does is allowed. "You offered something and it was not
// accepted" is a different fact from "you offered nothing", and only the
// caller can act on it.
func (s *Server) identityOrRefusal(r *http.Request) (api.Identity, string) {
	if s.deps.Identify == nil {
		return api.Anonymous(), ""
	}
	id, err := s.deps.Identify(r)
	if err == nil {
		return id, ""
	}
	if r.Header.Get("Authorization") == "" {
		return api.Anonymous(), ""
	}
	return api.Anonymous(), err.Error()
}

// isAdmin reports whether an identity may change the configuration. It is
// the same question the access engine answers for sys/config, asked once so
// the console can show or hide what it would otherwise offer and then refuse.
func (s *Server) isAdmin(id api.Identity) bool {
	return s.allows(id, config.SysConfig, access.CapUpdate).Allowed
}

// allows asks the access engine. Every authorization decision in this API
// goes through here, so "why was this refused" has exactly one answer and
// one place to read it from.
func (s *Server) allows(id api.Identity, path string, want access.Capability) access.Decision {
	engine := s.deps.Access()
	if engine == nil {
		return access.Decision{Reason: "access rules are not loaded"}
	}
	return engine.Explain(access.Identity{
		Kind:        string(id.Kind),
		Subject:     id.Subject,
		Issuer:      id.Issuer,
		ProjectPath: id.ProjectPath,
		Ref:         id.Ref,
	}, path, want)
}

// require answers the request itself when the caller may not do this.
func (s *Server) require(w http.ResponseWriter, r *http.Request,
	path string, want access.Capability) (api.Identity, bool) {
	id, refusal := s.identityOrRefusal(r)
	d := s.allows(id, path, want)
	if d.Allowed {
		return id, true
	}
	if id.IsAnonymous() {
		reason := d.Reason
		if refusal != "" {
			reason = "the credential you offered was not accepted: " + refusal
		}
		s.writeError(w, http.StatusUnauthorized, reason)
		return id, false
	}
	s.audit.Warn("request denied",
		"identity", id.String(), "method", r.Method, "path", r.URL.Path,
		"access_path", path, "capability", string(want), "reason", d.Reason, "site", s.site)
	s.writeError(w, http.StatusForbidden, d.Reason)
	return id, false
}

// requireIdentity answers the request itself when the caller is a stranger.
//
// It guards the operational read surface — replication, conflicts,
// quarantine. None of it is a secret from the people who run the registry,
// but all of it describes the deployment rather than the packages, and a
// client that only downloads public artifacts never needs to see it.
func (s *Server) requireIdentity(w http.ResponseWriter, r *http.Request, path string) bool {
	_, ok := s.require(w, r, path, access.CapRead)
	return ok
}

// requireAdmin is require for the configuration document, which is what
// "administrator" has always meant here.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) (api.Identity, bool) {
	return s.require(w, r, config.SysConfig, access.CapUpdate)
}

// ---------------------------------------------------------------------------
// Whole-document endpoints

// ConfigResponse carries the document and its version.
type ConfigResponse struct {
	Version  string `json:"version"`
	Source   string `json:"source"`
	Writable bool   `json:"writable"`
	Document string `json:"document"`
}

// handleGetConfig returns the active document. Reading configuration is an
// administrator action too: it names upstreams, peers and permissions.
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	raw, version, err := s.manager.Document(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("ETag", `"`+version+`"`)
	writeJSON(w, http.StatusOK, ConfigResponse{
		Version:  version,
		Source:   s.manager.Source().Describe(),
		Writable: s.manager.Source().Writable(),
		Document: string(raw),
	})
}

// handlePutConfig replaces the whole document.
func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxDocument))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	ifMatch := trimETag(r.Header.Get("If-Match"))

	version, err := s.replaceDocument(r.Context(), raw, ifMatch)
	if err != nil {
		s.writeConfigError(w, err)
		return
	}
	s.audit.Info("configuration replaced",
		"identity", id.String(), "version", version, "site", s.site)
	w.Header().Set("ETag", `"`+version+`"`)
	writeJSON(w, http.StatusOK, map[string]string{"version": version})
}

// replaceDocument validates and stores a document under the cross-replica
// lock, so two replicas cannot interleave read-modify-write cycles.
func (s *Server) replaceDocument(ctx context.Context, raw []byte, ifMatch string) (string, error) {
	if s.db == nil {
		// Without a lock two replicas would lose each other's edits. Say
		// so rather than corrupting the document.
		return "", fmt.Errorf("%w: configuration writes need a database for the cross-replica lock",
			api.ErrUnavailable)
	}
	var version string
	err := s.db.WithLock(ctx, configLockKey, func(ctx context.Context) error {
		var err error
		_, version, err = s.manager.Update(ctx, raw, ifMatch)
		return err
	})
	return version, err
}

// mutateDocument applies fn to the parsed document and stores the result.
// Every per-resource endpoint goes through it, so they all get the same
// validation, locking and version handling.
func (s *Server) mutateDocument(ctx context.Context, ifMatch string,
	fn func(doc *yaml.Node) error) (string, error) {
	if s.db == nil {
		return "", fmt.Errorf("%w: configuration writes need a database for the cross-replica lock",
			api.ErrUnavailable)
	}
	var version string
	err := s.db.WithLock(ctx, configLockKey, func(ctx context.Context) error {
		raw, current, err := s.manager.Document(ctx)
		if err != nil {
			return err
		}
		if ifMatch != "" && ifMatch != current {
			return fmt.Errorf("%w (have %s, expected %s)", config.ErrVersionConflict, current, ifMatch)
		}

		var doc yaml.Node
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("parse current document: %w", err)
		}
		if err := fn(&doc); err != nil {
			return err
		}
		updated, err := yaml.Marshal(&doc)
		if err != nil {
			return fmt.Errorf("encode document: %w", err)
		}
		// current, not ifMatch: the lock holder read it a moment ago, and
		// this keeps the write conditional even when the caller sent none.
		_, version, err = s.manager.Update(ctx, updated, current)
		return err
	})
	return version, err
}

// ---------------------------------------------------------------------------
// Helpers

func trimETag(v string) string {
	v = trimQuotes(v)
	if v == "*" {
		return ""
	}
	return v
}

func trimQuotes(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		return v[1 : len(v)-1]
	}
	return v
}

// writeConfigError maps configuration failures onto the status a client can
// act on: a conflict is retryable, a validation failure is not.
func (s *Server) writeConfigError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, config.ErrVersionConflict):
		s.writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, config.ErrReadOnlySource):
		s.writeError(w, http.StatusConflict,
			"the configuration source is read-only; switch config_source.type to store to manage it through the API: "+err.Error())
	case errors.Is(err, api.ErrUnavailable):
		s.writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, api.ErrNotFound):
		s.writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, api.ErrBadRequest):
		s.writeError(w, http.StatusBadRequest, err.Error())
	default:
		// A validation failure: the document was rejected before anything
		// was written, so the message is the useful part.
		s.writeError(w, http.StatusUnprocessableEntity, err.Error())
	}
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.logger.Error("admin api error", "error", err)
	s.writeError(w, http.StatusInternalServerError, "internal error")
}

// ErrorResponse is the one error shape the API uses.
type ErrorResponse struct {
	Error string `json:"error"`
}

func (s *Server) writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, ErrorResponse{Error: msg})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// decodeBody reads a JSON request body.
func decodeBody(r *http.Request, out any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxDocument))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	// Unknown fields are an error, not noise: a misspelt key that is
	// silently dropped becomes a setting the operator believes is applied.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("parse body: %w", err)
	}
	return nil
}

// yamlDocument renders a value as a YAML node for splicing into the
// document.
func yamlDocument(v any) (*yaml.Node, error) {
	raw, err := yaml.Marshal(v)
	if err != nil {
		return nil, err
	}
	var node yaml.Node
	if err := yaml.Unmarshal(raw, &node); err != nil {
		return nil, err
	}
	if len(node.Content) == 0 {
		return nil, errors.New("empty value")
	}
	return node.Content[0], nil
}
