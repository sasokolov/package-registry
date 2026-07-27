package adminapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sasokolov/package-registry/core/api"
	"github.com/sasokolov/package-registry/core/auth"
	"github.com/sasokolov/package-registry/core/repl"
	"github.com/sasokolov/package-registry/core/state"
)

// Operator actions the console offers. Each one calls the SAME code path
// the CLI does, so there is one implementation of every decision that
// changes what the registry serves — a second implementation behind a
// button is how a console and a command line drift apart.

// TokenInfo describes a token without ever revealing it (invariant 12).
type TokenInfo struct {
	Name       string     `json:"name"`
	HashPrefix string     `json:"hash_prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if s.db == nil {
		s.writeError(w, http.StatusServiceUnavailable, "tokens need a database")
		return
	}
	rows, err := s.db.Pool().Query(r.Context(),
		"SELECT name, hash, created_at, revoked_at FROM tokens ORDER BY name")
	if err != nil {
		s.fail(w, err)
		return
	}
	defer rows.Close()

	tokens := []TokenInfo{}
	for rows.Next() {
		var t TokenInfo
		var hash string
		if err := rows.Scan(&t.Name, &hash, &t.CreatedAt, &t.RevokedAt); err != nil {
			s.fail(w, err)
			return
		}
		// Eight characters of the hash: enough to correlate with an audit
		// line, useless as a credential.
		if len(hash) >= 8 {
			t.HashPrefix = hash[:8]
		}
		tokens = append(tokens, t)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": tokens})
}

// handleCreateToken issues a token. The secret is in the response and
// nowhere else — it is never stored, logged or retrievable afterwards.
func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	if s.db == nil {
		s.writeError(w, http.StatusServiceUnavailable, "tokens need a database")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Name == "" {
		s.writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	secret, err := auth.NewTokens(s.db).Create(r.Context(), body.Name)
	if err != nil {
		s.writeConfigError(w, err)
		return
	}
	s.audit.Info("token issued via the API",
		"identity", id.String(), "token_name", body.Name, "site", s.site)
	writeJSON(w, http.StatusCreated, map[string]string{
		"name":   body.Name,
		"secret": secret,
		"note":   "the secret is shown once and is not recoverable",
	})
}

// handleRevokeToken revokes a token everywhere.
func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	if s.db == nil {
		s.writeError(w, http.StatusServiceUnavailable, "tokens need a database")
		return
	}
	name := chi.URLParam(r, "name")

	tx, err := s.db.Begin(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()

	hash, err := auth.RevokeTx(r.Context(), tx, name)
	if err != nil {
		if errors.Is(err, api.ErrNotFound) {
			s.writeError(w, http.StatusNotFound, err.Error())
			return
		}
		s.fail(w, err)
		return
	}
	if s.manager.Current().Replication.Enabled {
		if err := repl.NewWriter(s.site).AppendTokenRevoke(r.Context(), tx, hash); err != nil {
			s.fail(w, err)
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.fail(w, err)
		return
	}
	s.audit.Warn("token revoked via the API",
		"identity", id.String(), "token_name", name, "hash_prefix", hash[:8], "site", s.site)
	writeJSON(w, http.StatusOK, map[string]string{"name": name, "hash_prefix": hash[:8]})
}

// QuarantineEntry is one active block.
type QuarantineEntry struct {
	Feed       string    `json:"feed"`
	Coordinate string    `json:"coordinate"`
	Reason     string    `json:"reason"`
	Detail     string    `json:"detail,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

func (s *Server) handleListQuarantine(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		s.writeError(w, http.StatusServiceUnavailable, "quarantine needs a database")
		return
	}
	rows, err := s.db.Pool().Query(r.Context(), `
		SELECT feed, coordinate, reason, detail, created_at
		  FROM quarantine WHERE released_at IS NULL
		 ORDER BY feed, coordinate, reason`)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer rows.Close()

	entries := []QuarantineEntry{}
	for rows.Next() {
		var e QuarantineEntry
		if err := rows.Scan(&e.Feed, &e.Coordinate, &e.Reason, &e.Detail, &e.CreatedAt); err != nil {
			s.fail(w, err)
			return
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"quarantine": entries})
}

// handleQuarantine blocks or releases a coordinate, through the same
// journalled path the CLI uses.
func (s *Server) handleQuarantine(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	if s.db == nil {
		s.writeError(w, http.StatusServiceUnavailable, "quarantine needs a database")
		return
	}
	var body struct {
		Feed       string `json:"feed"`
		Coordinate string `json:"coordinate"`
		Reason     string `json:"reason"`
		Detail     string `json:"detail"`
		Active     *bool  `json:"active"`
	}
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Feed == "" || body.Coordinate == "" {
		s.writeError(w, http.StatusBadRequest, "feed and coordinate are required")
		return
	}
	if body.Reason == "" {
		body.Reason = "manual"
	}
	if body.Reason == "cross_site_conflict" {
		s.writeError(w, http.StatusBadRequest,
			"a conflict block is derived from recorded conflicts; resolve the conflict instead")
		return
	}
	active := true
	if body.Active != nil {
		active = *body.Active
	}

	tx, err := s.db.Begin(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()

	// One stamp for the local write and the journal entry, so this site
	// orders the decision exactly as its peers will.
	var stamp state.HLC
	if s.manager.Current().Replication.Enabled {
		writer := repl.NewWriter(s.site)
		var entry state.JournalEntry
		if active {
			entry, err = writer.AppendQuarantineEntry(r.Context(), tx,
				body.Feed, body.Coordinate, body.Reason, body.Detail)
		} else {
			entry, err = writer.AppendQuarantineReleaseEntry(r.Context(), tx,
				body.Feed, body.Coordinate, body.Reason)
		}
		if err != nil {
			s.fail(w, err)
			return
		}
		stamp = entry.HLC
	} else {
		var wall, logical int64
		if err := tx.QueryRow(r.Context(),
			"SELECT hlc_wall, hlc_logical FROM repl_hlc_now()").Scan(&wall, &logical); err != nil {
			s.fail(w, err)
			return
		}
		stamp = state.HLC{Wall: wall, Logical: logical}
	}
	if err := repl.ApplyQuarantineDecisionTx(r.Context(), tx,
		body.Feed, body.Coordinate, body.Reason, body.Detail, active, stamp); err != nil {
		s.fail(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.fail(w, err)
		return
	}

	s.audit.Warn("quarantine decision via the API",
		"identity", id.String(), "feed", body.Feed, "coordinate", body.Coordinate,
		"reason", body.Reason, "active", active, "site", s.site)
	writeJSON(w, http.StatusOK, map[string]any{
		"feed": body.Feed, "coordinate": body.Coordinate,
		"reason": body.Reason, "active": active,
	})
}

// ConflictEntry is one recorded cross-site conflict.
type ConflictEntry struct {
	Feed        string    `json:"feed"`
	Path        string    `json:"path"`
	Coordinate  string    `json:"coordinate"`
	Canonical   string    `json:"canonical_sha256"`
	Other       string    `json:"other_sha256"`
	SiteA       string    `json:"canonical_site"`
	SiteB       string    `json:"other_site"`
	DetectedAt  time.Time `json:"detected_at"`
	Resolved    bool      `json:"resolved"`
	ResolvedSHA string    `json:"resolved_sha256,omitempty"`
}

func (s *Server) handleListConflicts(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		s.writeError(w, http.StatusServiceUnavailable, "conflicts need a database")
		return
	}
	openOnly := r.URL.Query().Get("all") != "true"
	rows, err := s.db.ListConflicts(r.Context(), openOnly)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []ConflictEntry{}
	for _, c := range rows {
		out = append(out, ConflictEntry{
			Feed: c.Feed, Path: c.Path, Coordinate: c.Coordinate,
			Canonical: c.WinnerSHA, Other: c.LoserSHA,
			SiteA: c.WinnerSite, SiteB: c.LoserSite,
			DetectedAt: c.DetectedAt, Resolved: c.Resolved, ResolvedSHA: c.ResolvedSHA,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"conflicts": out})
}

// handleResolveConflict applies an operator's decision, through the same
// shared operation the CLI calls.
func (s *Server) handleResolveConflict(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	if s.db == nil {
		s.writeError(w, http.StatusServiceUnavailable, "resolving conflicts needs a database")
		return
	}
	var body struct {
		Feed       string `json:"feed"`
		Path       string `json:"path"`
		Coordinate string `json:"coordinate"`
		KeepSHA    string `json:"keep_sha256"`
	}
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Feed == "" || body.Path == "" || len(body.KeepSHA) != 64 {
		s.writeError(w, http.StatusBadRequest, "feed, path and a full keep_sha256 are required")
		return
	}
	if body.Coordinate == "" {
		// Recover it from the conflict record rather than trusting the
		// caller to restate it.
		conflicts, err := s.db.ListConflicts(r.Context(), true)
		if err != nil {
			s.fail(w, err)
			return
		}
		for _, c := range conflicts {
			if c.Feed == body.Feed && c.Path == body.Path {
				body.Coordinate = c.Coordinate
				break
			}
		}
		if body.Coordinate == "" {
			s.writeError(w, http.StatusNotFound, "no open conflict for that coordinate")
			return
		}
	}

	if err := repl.ResolveConflict(r.Context(), s.db, s.projector(), s.site,
		body.Feed, body.Path, body.Coordinate, body.KeepSHA, id.String()); err != nil {
		s.writeConfigError(w, err)
		return
	}
	s.audit.Warn("conflict resolved via the API",
		"identity", id.String(), "feed", body.Feed, "path", body.Path,
		"keep_sha256", body.KeepSHA, "site", s.site)
	writeJSON(w, http.StatusOK, map[string]string{
		"feed": body.Feed, "path": body.Path, "keep_sha256": body.KeepSHA,
	})
}

// ReplicationStatus is what the console shows for geo.
type ReplicationStatus struct {
	Enabled bool             `json:"enabled"`
	Site    string           `json:"site"`
	Cursors []CursorInfo     `json:"cursors"`
	Peers   []PeerIdentity   `json:"peers"`
	Parked  int              `json:"parked"`
	Heads   map[string]int64 `json:"heads"`
}

// CursorInfo is one replication stream.
type CursorInfo struct {
	Peer       string    `json:"peer"`
	Origin     string    `json:"origin"`
	AppliedSeq int64     `json:"applied_seq"`
	DurableSeq int64     `json:"durable_seq"`
	LastOKAt   time.Time `json:"last_ok_at"`
	LastError  string    `json:"last_error,omitempty"`
}

// PeerIdentity is a pinned peer.
type PeerIdentity struct {
	Peer     string    `json:"peer"`
	UUID     string    `json:"uuid"`
	LastSeen time.Time `json:"last_seen"`
}

func (s *Server) handleReplication(w http.ResponseWriter, r *http.Request) {
	cfg := s.manager.Current()
	out := ReplicationStatus{
		Enabled: cfg.Replication.Enabled,
		Site:    cfg.Site.Name,
		Cursors: []CursorInfo{},
		Peers:   []PeerIdentity{},
		Heads:   map[string]int64{},
	}
	if s.db == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}

	if cursors, err := s.db.ListCursors(r.Context()); err == nil {
		for _, c := range cursors {
			out.Cursors = append(out.Cursors, CursorInfo{
				Peer: c.Peer, Origin: c.Origin,
				AppliedSeq: c.AppliedSeq, DurableSeq: c.DurableSeq,
				LastOKAt: c.LastOKAt, LastError: c.LastError,
			})
		}
	}
	if peers, err := s.db.ListPeerIdentities(r.Context()); err == nil {
		for _, p := range peers {
			out.Peers = append(out.Peers, PeerIdentity{Peer: p.Peer, UUID: p.UUID, LastSeen: p.LastSeen})
		}
	}
	if n, err := s.db.CountParked(r.Context()); err == nil {
		out.Parked = n
	}
	if origins, err := s.db.KnownOrigins(r.Context()); err == nil {
		for _, origin := range origins {
			if head, _, err := s.db.JournalHead(r.Context(), origin); err == nil {
				out.Heads[origin] = head
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}
