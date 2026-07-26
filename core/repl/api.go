package repl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/sasokolov/package-registry/core/api"
	"github.com/sasokolov/package-registry/core/state"
)

// InternalPrefix is the base path of the replication API. It is served on a
// dedicated listener and never mounted on the public router (invariant 14).
const InternalPrefix = "/internal/replication/v1"

// StatusResponse is the peer handshake and lag source.
type StatusResponse struct {
	Site    string            `json:"site"`
	UUID    string            `json:"uuid"`
	Heads   map[string]int64  `json:"heads"`   // origin -> newest sequence
	Oldest  map[string]int64  `json:"oldest"`  // origin -> oldest retained sequence
	Digests map[string]string `json:"digests"` // feed -> manifest-set digest
}

// JournalResponse is a page of journal entries.
type JournalResponse struct {
	Entries []state.JournalEntry `json:"entries"`
	// Head is the newest sequence of this origin, so a puller knows its lag
	// without a second request.
	Head int64 `json:"head"`
}

// SnapshotResponse bootstraps a new site: the full hosted manifest set plus
// the watermark it corresponds to.
type SnapshotResponse struct {
	Site       string           `json:"site"`
	Manifests  []ManifestPut    `json:"manifests"`
	Revoked    []string         `json:"revoked_token_hashes"`
	Quarantine []QuarantineSet  `json:"quarantine"`
	Watermarks map[string]int64 `json:"watermarks"` // origin -> sequence
}

// Server exposes the internal replication API.
type Server struct {
	db      *state.DB
	store   api.BlobStore
	site    string
	uuid    string
	logger  *slog.Logger
	metrics *Metrics
	// authorize checks the caller's credentials and returns the peer name.
	authorize func(r *http.Request) (peer string, err error)
	// digests computes per-feed manifest-set digests for divergence
	// detection (invariant 16).
	digests func(ctx context.Context) map[string]string
}

// ServerOptions wires the internal API.
type ServerOptions struct {
	DB        *state.DB
	Store     api.BlobStore
	Site      string
	UUID      string
	Logger    *slog.Logger
	Metrics   *Metrics
	Authorize func(r *http.Request) (string, error)
	Digests   func(ctx context.Context) map[string]string
}

// NewServer builds the internal API server.
func NewServer(o ServerOptions) *Server {
	s := &Server{
		db: o.DB, store: o.Store, site: o.Site, uuid: o.UUID,
		logger: o.Logger, metrics: o.Metrics, authorize: o.Authorize, digests: o.Digests,
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	if s.authorize == nil {
		// Fail closed: an unconfigured API serves nobody.
		s.authorize = func(*http.Request) (string, error) {
			return "", errors.New("replication API is not configured for authentication")
		}
	}
	return s
}

// Handler returns the internal API router.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Route(InternalPrefix, func(r chi.Router) {
		r.Use(s.authMiddleware)
		r.Get("/status", s.handleStatus)
		r.Get("/journal", s.handleJournal)
		r.Get("/blobs/sha256/{digest}", s.handleBlob)
		r.Get("/manifest", s.handleManifest)
		r.Get("/snapshot", s.handleSnapshot)
		r.Post("/nudge", s.handleNudge)
	})
	return r
}

type peerKey struct{}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, err := s.authorize(r)
		if err != nil {
			s.logger.Warn("replication API request rejected",
				"remote", r.RemoteAddr, "path", r.URL.Path, "error", err)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), peerKey{}, peer)))
	})
}

func peerOf(r *http.Request) string {
	peer, _ := r.Context().Value(peerKey{}).(string)
	return peer
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	origins, err := s.db.KnownOrigins(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	if len(origins) == 0 {
		origins = []string{s.site}
	}
	resp := StatusResponse{
		Site:   s.site,
		UUID:   s.uuid,
		Heads:  map[string]int64{},
		Oldest: map[string]int64{},
	}
	for _, origin := range origins {
		head, oldest, err := s.db.JournalHead(ctx, origin)
		if err != nil {
			s.fail(w, err)
			return
		}
		resp.Heads[origin] = head
		resp.Oldest[origin] = oldest
	}
	if s.digests != nil {
		resp.Digests = s.digests(ctx)
	}
	writeJSON(w, resp)
}

func (s *Server) handleJournal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	origin := r.URL.Query().Get("origin")
	if origin == "" {
		origin = s.site
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

	head, oldest, err := s.db.JournalHead(ctx, origin)
	if err != nil {
		s.fail(w, err)
		return
	}
	// The cursor is behind what we still retain: the peer must re-bootstrap
	// from a snapshot instead of silently missing events.
	if oldest > 0 && after > 0 && after < oldest-1 {
		http.Error(w, "cursor is beyond the retained journal; resync from /snapshot", http.StatusGone)
		return
	}
	entries, err := s.db.ReadJournal(ctx, origin, after, limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, JournalResponse{Entries: entries, Head: head})
}

var digestRE = "0123456789abcdef"

// handleBlob streams a blob by digest. The key is the checksum, so the
// transfer is self-verifying on the receiving end (invariant 5).
func (s *Server) handleBlob(w http.ResponseWriter, r *http.Request) {
	digest := chi.URLParam(r, "digest")
	if len(digest) != 64 || strings.Trim(digest, digestRE) != "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	rc, info, err := s.store.Get(r.Context(), "blobs/sha256/"+digest)
	if err != nil {
		if errors.Is(err, api.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		s.fail(w, err)
		return
	}
	defer func() { _ = rc.Close() }()
	w.Header().Set("Content-Type", "application/octet-stream")
	if info.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	}
	if _, err := io.Copy(w, rc); err != nil {
		s.logger.Debug("peer blob transfer interrupted",
			"peer", peerOf(r), "digest", short(digest), "error", err)
	}
}

// ManifestResponse answers a peer's read-through query for one hosted
// coordinate (used while replication catches up).
type ManifestResponse struct {
	Feed   string `json:"feed"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	feed := r.URL.Query().Get("feed")
	path := r.URL.Query().Get("path")
	if feed == "" || path == "" {
		http.Error(w, "feed and path are required", http.StatusBadRequest)
		return
	}
	rows, err := s.db.ListHosted(r.Context(), feed, path)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, row := range rows {
		if row.Path == path {
			writeJSON(w, ManifestResponse{
				Feed: row.Feed, Path: row.Path, SHA256: row.SHA256, Size: row.Size,
			})
			return
		}
	}
	http.Error(w, "not found", http.StatusNotFound)
}

// handleSnapshot returns the full replicable state for bootstrapping.
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	snap := SnapshotResponse{Site: s.site, Watermarks: map[string]int64{}}

	rows, err := s.db.ListHosted(ctx, "", "")
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, row := range rows {
		snap.Manifests = append(snap.Manifests, ManifestPut{
			Feed: row.Feed, Path: row.Path, Coord: row.Coordinate,
			SHA256: row.SHA256, Size: row.Size,
			Checksums: row.Checksums, Metadata: row.Metadata,
			Mutable: row.Mutable, Publisher: row.PublishedBy,
		})
	}

	origins, err := s.db.KnownOrigins(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, origin := range origins {
		head, _, err := s.db.JournalHead(ctx, origin)
		if err != nil {
			s.fail(w, err)
			return
		}
		snap.Watermarks[origin] = head
	}
	writeJSON(w, snap)
}

// handleNudge is a hint that new events exist; the puller polls anyway, so
// this only shortens the delay.
func (s *Server) handleNudge(w http.ResponseWriter, r *http.Request) {
	s.logger.Debug("nudge received", "peer", peerOf(r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.logger.Error("replication API error", "error", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(body)
}

// FeedDigest is the divergence detector: a stable digest of a feed's hosted
// manifest set. Two sites in agreement produce the same string; a lasting
// difference is an alert, not silence (invariant 16).
func FeedDigest(rows []state.HostedRow) string {
	h := sha256.New()
	for _, r := range rows {
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00", r.Feed, r.Path, r.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
