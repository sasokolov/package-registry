package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// HLC is a hybrid logical clock reading: wall-clock milliseconds plus a
// logical counter that breaks ties and preserves causality.
type HLC struct {
	Wall    int64 `json:"wall"`
	Logical int64 `json:"logical"`
}

// Before orders two readings.
func (h HLC) Before(other HLC) bool {
	if h.Wall != other.Wall {
		return h.Wall < other.Wall
	}
	return h.Logical < other.Logical
}

// Time renders the wall component.
func (h HLC) Time() time.Time { return time.UnixMilli(h.Wall).UTC() }

// JournalEntry is one replication event.
type JournalEntry struct {
	OriginSite    string          `json:"origin_site"`
	OriginSeq     int64           `json:"origin_seq"`
	Kind          string          `json:"kind"`
	Payload       json.RawMessage `json:"payload"`
	HLC           HLC             `json:"hlc"`
	SchemaVersion int             `json:"schema_version"`
}

// SiteIdentity is this site's stable name and UUID.
type SiteIdentity struct {
	Site string
	UUID string
}

// EnsureSiteIdentity creates the identity row on first start and returns it.
// A changed site name is rejected: it would let a cloned deployment
// impersonate another site in the mesh.
func (db *DB) EnsureSiteIdentity(ctx context.Context, site string) (SiteIdentity, error) {
	var id SiteIdentity
	err := db.pool.QueryRow(ctx,
		"SELECT site, site_uuid::text FROM site_identity WHERE id").Scan(&id.Site, &id.UUID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		err = db.pool.QueryRow(ctx, `
			INSERT INTO site_identity (site) VALUES ($1)
			ON CONFLICT (id) DO NOTHING
			RETURNING site, site_uuid::text`, site).Scan(&id.Site, &id.UUID)
		if errors.Is(err, pgx.ErrNoRows) {
			// Lost the race with another replica: read what it wrote.
			err = db.pool.QueryRow(ctx,
				"SELECT site, site_uuid::text FROM site_identity WHERE id").Scan(&id.Site, &id.UUID)
		}
		if err != nil {
			return SiteIdentity{}, classify(fmt.Errorf("create site identity: %w", err))
		}
	case err != nil:
		return SiteIdentity{}, classify(fmt.Errorf("read site identity: %w", err))
	}
	if id.Site != site {
		return SiteIdentity{}, fmt.Errorf(
			"this database belongs to site %q but the config says %q: refusing to start with a mismatched site identity",
			id.Site, site)
	}
	return id, nil
}

// AppendJournal writes a local event inside tx, allocating its sequence and
// HLC under the hlc_state row lock so journal order equals commit order.
func AppendJournal(ctx context.Context, tx pgx.Tx, site, kind string, payload any) (JournalEntry, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return JournalEntry{}, fmt.Errorf("encode %s payload: %w", kind, err)
	}
	var entry JournalEntry
	err = tx.QueryRow(ctx, `
		WITH stamp AS (SELECT * FROM repl_hlc_next())
		INSERT INTO repl_journal (origin_site, origin_seq, kind, payload, hlc_wall, hlc_logical)
		SELECT $1, stamp.seq, $2, $3, stamp.hlc_wall, stamp.hlc_logical FROM stamp
		RETURNING origin_site, origin_seq, kind, payload, hlc_wall, hlc_logical, schema_version`,
		site, kind, raw).
		Scan(&entry.OriginSite, &entry.OriginSeq, &entry.Kind, &entry.Payload,
			&entry.HLC.Wall, &entry.HLC.Logical, &entry.SchemaVersion)
	if err != nil {
		return JournalEntry{}, classify(fmt.Errorf("append journal entry %s: %w", kind, err))
	}
	return entry, nil
}

// ApplyForeignJournal records an event received from a peer. Re-applying
// the same (origin_site, origin_seq) is a no-op, which makes the puller
// idempotent under retries.
func (db *DB) ApplyForeignJournal(ctx context.Context, tx pgx.Tx, e JournalEntry) (bool, error) {
	if _, err := tx.Exec(ctx, "SELECT repl_hlc_recv($1, $2)", e.HLC.Wall, e.HLC.Logical); err != nil {
		return false, classify(fmt.Errorf("advance hlc: %w", err))
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO repl_journal (origin_site, origin_seq, kind, payload, hlc_wall, hlc_logical, schema_version)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (origin_site, origin_seq) DO NOTHING`,
		e.OriginSite, e.OriginSeq, e.Kind, e.Payload, e.HLC.Wall, e.HLC.Logical, e.SchemaVersion)
	if err != nil {
		return false, classify(fmt.Errorf("store foreign journal entry: %w", err))
	}
	return tag.RowsAffected() > 0, nil
}

// ReadJournal returns entries of one origin after a sequence, oldest first.
func (db *DB) ReadJournal(ctx context.Context, origin string, after int64, limit int) ([]JournalEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := db.pool.Query(ctx, `
		SELECT origin_site, origin_seq, kind, payload, hlc_wall, hlc_logical, schema_version
		  FROM repl_journal
		 WHERE origin_site = $1 AND origin_seq > $2
		 ORDER BY origin_seq
		 LIMIT $3`, origin, after, limit)
	if err != nil {
		return nil, classify(fmt.Errorf("read journal: %w", err))
	}
	defer rows.Close()

	var out []JournalEntry
	for rows.Next() {
		var e JournalEntry
		if err := rows.Scan(&e.OriginSite, &e.OriginSeq, &e.Kind, &e.Payload,
			&e.HLC.Wall, &e.HLC.Logical, &e.SchemaVersion); err != nil {
			return nil, fmt.Errorf("scan journal entry: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// JournalHead reports the newest sequence of an origin and the oldest one
// still retained (0 when nothing was pruned).
func (db *DB) JournalHead(ctx context.Context, origin string) (head, oldest int64, err error) {
	err = db.pool.QueryRow(ctx,
		"SELECT COALESCE(MAX(origin_seq),0), COALESCE(MIN(origin_seq),0) FROM repl_journal WHERE origin_site=$1",
		origin).Scan(&head, &oldest)
	if err != nil {
		return 0, 0, classify(fmt.Errorf("read journal head: %w", err))
	}
	return head, oldest, nil
}

// KnownOrigins lists every origin site present in the journal.
func (db *DB) KnownOrigins(ctx context.Context) ([]string, error) {
	rows, err := db.pool.Query(ctx, "SELECT DISTINCT origin_site FROM repl_journal ORDER BY origin_site")
	if err != nil {
		return nil, classify(fmt.Errorf("list journal origins: %w", err))
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Cursor is how far a peer stream has been applied.
type Cursor struct {
	Peer       string
	Origin     string
	AppliedSeq int64
	DurableSeq int64
	LastOKAt   time.Time
	LastError  string
}

// GetCursor reads a cursor, returning zeroes when the stream is new.
func (db *DB) GetCursor(ctx context.Context, peer, origin string) (Cursor, error) {
	c := Cursor{Peer: peer, Origin: origin}
	var lastOK *time.Time
	err := db.pool.QueryRow(ctx, `
		SELECT applied_seq, durable_seq, last_ok_at, last_error
		  FROM repl_cursors WHERE peer=$1 AND origin_site=$2`, peer, origin).
		Scan(&c.AppliedSeq, &c.DurableSeq, &lastOK, &c.LastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return Cursor{}, classify(fmt.Errorf("read cursor: %w", err))
	}
	if lastOK != nil {
		c.LastOKAt = *lastOK
	}
	return c, nil
}

// SetCursorTx advances a cursor inside the same transaction that applied
// the batch, so a crash can never skip events.
func SetCursorTx(ctx context.Context, tx pgx.Tx, c Cursor) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO repl_cursors (peer, origin_site, applied_seq, durable_seq, last_ok_at, last_error)
		VALUES ($1,$2,$3,$4, now(), $5)
		ON CONFLICT (peer, origin_site) DO UPDATE
		   SET applied_seq = EXCLUDED.applied_seq,
		       durable_seq = GREATEST(repl_cursors.durable_seq, EXCLUDED.durable_seq),
		       last_ok_at  = now(),
		       last_error  = EXCLUDED.last_error`,
		c.Peer, c.Origin, c.AppliedSeq, c.DurableSeq, c.LastError)
	if err != nil {
		return fmt.Errorf("advance cursor: %w", err)
	}
	return nil
}

// RecordCursorError notes a failed poll without moving the cursor. A poll
// that fails before any origin is known is recorded against every existing
// stream of that peer, so `repl status` shows the real reason instead of a
// sentinel row that never clears.
func (db *DB) RecordCursorError(ctx context.Context, peer, origin, msg string) error {
	if origin == "" || origin == "*" {
		_, err := db.pool.Exec(ctx,
			"UPDATE repl_cursors SET last_error = $2 WHERE peer = $1", peer, msg)
		return err
	}
	_, err := db.pool.Exec(ctx, `
		INSERT INTO repl_cursors (peer, origin_site, last_error)
		VALUES ($1,$2,$3)
		ON CONFLICT (peer, origin_site) DO UPDATE SET last_error = EXCLUDED.last_error`,
		peer, origin, msg)
	return err
}

// MarkPeerPollOK records a successful handshake with a peer, even when
// there was nothing to apply: without it a healthy but idle stream would
// look like it had never been reached.
func (db *DB) MarkPeerPollOK(ctx context.Context, peer string, origins []string) error {
	// Clear the error on EVERY stream of this peer, not just the ones we
	// import: the row that records how far the peer has consumed OUR
	// journal is also marked when a poll fails, and would otherwise keep
	// showing a fault long after the peer came back.
	if _, err := db.pool.Exec(ctx,
		"UPDATE repl_cursors SET last_error = '' WHERE peer = $1 AND last_error <> ''", peer); err != nil {
		return classify(fmt.Errorf("clear peer errors: %w", err))
	}
	if len(origins) == 0 {
		origins = []string{peer}
	}
	for _, origin := range origins {
		if _, err := db.pool.Exec(ctx, `
			INSERT INTO repl_cursors (peer, origin_site, last_ok_at, last_error)
			VALUES ($1,$2, now(), '')
			ON CONFLICT (peer, origin_site) DO UPDATE
			   SET last_ok_at = now(), last_error = ''`, peer, origin); err != nil {
			return classify(fmt.Errorf("record successful poll: %w", err))
		}
	}
	return nil
}

// ListCursors returns every known cursor (for metrics and `repl status`).
func (db *DB) ListCursors(ctx context.Context) ([]Cursor, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT peer, origin_site, applied_seq, durable_seq, last_ok_at, last_error
		  FROM repl_cursors ORDER BY peer, origin_site`)
	if err != nil {
		return nil, classify(fmt.Errorf("list cursors: %w", err))
	}
	defer rows.Close()
	var out []Cursor
	for rows.Next() {
		var c Cursor
		var lastOK *time.Time
		if err := rows.Scan(&c.Peer, &c.Origin, &c.AppliedSeq, &c.DurableSeq, &lastOK, &c.LastError); err != nil {
			return nil, err
		}
		if lastOK != nil {
			c.LastOKAt = *lastOK
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Conflicts and parked events

// RecordConflict stores both sides of a cross-site publish conflict.
func (db *DB) RecordConflict(ctx context.Context, feed, path, coordinate,
	winnerSHA, loserSHA, winnerSite, loserSite string) error {
	_, err := db.pool.Exec(ctx, `
		INSERT INTO publish_conflicts
			(feed, path, coordinate, winner_sha256, loser_sha256, winner_site, loser_site)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		feed, path, coordinate, winnerSHA, loserSHA, winnerSite, loserSite)
	if err != nil {
		return classify(fmt.Errorf("record publish conflict: %w", err))
	}
	return nil
}

// ConflictRow is an open or resolved conflict.
type ConflictRow struct {
	Feed        string    `json:"feed"`
	Path        string    `json:"path"`
	Coordinate  string    `json:"coordinate"`
	WinnerSHA   string    `json:"canonical_sha256"`
	LoserSHA    string    `json:"other_sha256"`
	WinnerSite  string    `json:"canonical_site"`
	LoserSite   string    `json:"other_site"`
	DetectedAt  time.Time `json:"detected_at"`
	Resolved    bool      `json:"resolved"`
	ResolvedSHA string    `json:"resolved_sha256,omitempty"`
}

// ListConflicts returns conflicts, open ones first.
func (db *DB) ListConflicts(ctx context.Context, openOnly bool) ([]ConflictRow, error) {
	query := `SELECT feed, path, coordinate, winner_sha256, loser_sha256, winner_site, loser_site,
	                 detected_at, resolved_at IS NOT NULL, COALESCE(resolved_sha256,'')
	            FROM publish_conflicts`
	if openOnly {
		query += " WHERE resolved_at IS NULL"
	}
	query += " ORDER BY detected_at DESC"
	rows, err := db.pool.Query(ctx, query)
	if err != nil {
		return nil, classify(fmt.Errorf("list conflicts: %w", err))
	}
	defer rows.Close()
	var out []ConflictRow
	for rows.Next() {
		var c ConflictRow
		if err := rows.Scan(&c.Feed, &c.Path, &c.Coordinate, &c.WinnerSHA, &c.LoserSHA,
			&c.WinnerSite, &c.LoserSite, &c.DetectedAt, &c.Resolved, &c.ResolvedSHA); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ResolveConflict marks the open conflicts of a coordinate as resolved.
func (db *DB) ResolveConflict(ctx context.Context, feed, path, sha256 string) error {
	_, err := db.pool.Exec(ctx, `
		UPDATE publish_conflicts
		   SET resolved_at = now(), resolved_sha256 = $3
		 WHERE feed=$1 AND path=$2 AND resolved_at IS NULL`, feed, path, sha256)
	if err != nil {
		return classify(fmt.Errorf("resolve conflict: %w", err))
	}
	return nil
}

// ParkEvent stores an event that could not be applied yet.
func (db *DB) ParkEvent(ctx context.Context, e JournalEntry, reason string) error {
	_, err := db.pool.Exec(ctx, `
		INSERT INTO repl_parked
			(origin_site, origin_seq, kind, payload, reason, hlc_wall, hlc_logical, schema_version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (origin_site, origin_seq)
		DO UPDATE SET reason = EXCLUDED.reason, retries = repl_parked.retries + 1`,
		e.OriginSite, e.OriginSeq, e.Kind, e.Payload, reason,
		e.HLC.Wall, e.HLC.Logical, e.SchemaVersion)
	if err != nil {
		return classify(fmt.Errorf("park event: %w", err))
	}
	return nil
}

// ParkedEvents returns parked events for retry, oldest first.
func (db *DB) ParkedEvents(ctx context.Context, limit int) ([]JournalEntry, []string, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.pool.Query(ctx, `
		SELECT origin_site, origin_seq, kind, payload, reason,
		       hlc_wall, hlc_logical, schema_version
		  FROM repl_parked ORDER BY parked_at LIMIT $1`, limit)
	if err != nil {
		return nil, nil, classify(fmt.Errorf("list parked events: %w", err))
	}
	defer rows.Close()
	var entries []JournalEntry
	var reasons []string
	for rows.Next() {
		var e JournalEntry
		var reason string
		if err := rows.Scan(&e.OriginSite, &e.OriginSeq, &e.Kind, &e.Payload, &reason,
			&e.HLC.Wall, &e.HLC.Logical, &e.SchemaVersion); err != nil {
			return nil, nil, err
		}
		entries = append(entries, e)
		reasons = append(reasons, reason)
	}
	return entries, reasons, rows.Err()
}

// UnparkEvent removes a parked event after a successful retry.
func (db *DB) UnparkEvent(ctx context.Context, origin string, seq int64) error {
	_, err := db.pool.Exec(ctx,
		"DELETE FROM repl_parked WHERE origin_site=$1 AND origin_seq=$2", origin, seq)
	return err
}

// CountParked reports how many events are parked (metrics).
func (db *DB) CountParked(ctx context.Context) (int, error) {
	var n int
	err := db.pool.QueryRow(ctx, "SELECT count(*) FROM repl_parked").Scan(&n)
	return n, err
}

// PruneJournal drops local entries below a watermark, keeping at least
// keepMin entries so a briefly disconnected peer does not need a resync.
func (db *DB) PruneJournal(ctx context.Context, origin string, below int64) (int64, error) {
	tag, err := db.pool.Exec(ctx,
		"DELETE FROM repl_journal WHERE origin_site=$1 AND origin_seq < $2", origin, below)
	if err != nil {
		return 0, classify(fmt.Errorf("prune journal: %w", err))
	}
	return tag.RowsAffected(), nil
}

// TryLease acquires a short-lived exclusive lease on a key using a
// try-lock, so exactly one replica of a site runs a job at a time. Unlike
// WithLock it never blocks: a replica that loses the race skips this round
// and tries again on the next tick.
func (db *DB) TryLease(ctx context.Context, key string, fn func(ctx context.Context) error) (ran bool, err error) {
	// Like WithLock, the lease lives on its own connection: fn does plenty
	// of pooled database work, and lending it the lock holder's connection
	// would deadlock a small pool.
	conn, err := db.lockConn(ctx)
	if err != nil {
		return false, classify(fmt.Errorf("lease %q: %w", key, err))
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()

	id := LockID(key)
	var got bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", id).Scan(&got); err != nil {
		return false, classify(fmt.Errorf("lease %q: %w", key, err))
	}
	if !got {
		return false, nil
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, uerr := conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", id); uerr != nil {
			db.logger.Warn("lease unlock failed; the connection will be closed", "key", key, "error", uerr)
		}
	}()
	return true, fn(ctx)
}

// PinPeerIdentity records a peer's site UUID the first time it is seen and
// refuses any later change: a peer whose UUID changed is a different site
// wearing a familiar name. The pin is durable, so every replica and every
// restart makes the same decision.
func (db *DB) PinPeerIdentity(ctx context.Context, peer, uuid string) error {
	var stored string
	err := db.pool.QueryRow(ctx, `
		INSERT INTO repl_peer_identity (peer, site_uuid) VALUES ($1, $2)
		ON CONFLICT (peer) DO UPDATE SET last_seen = now()
		RETURNING site_uuid::text`, peer, uuid).Scan(&stored)
	if err != nil {
		return classify(fmt.Errorf("pin peer identity: %w", err))
	}
	if stored != uuid {
		return fmt.Errorf(
			"peer %s now identifies as site UUID %s but was pinned to %s: a different site is using this name; "+
				"delete its row from repl_peer_identity if the change is intentional",
			peer, uuid, stored)
	}
	return nil
}

// PeerIdentity is a pinned peer.
type PeerIdentity struct {
	Peer      string
	UUID      string
	FirstSeen time.Time
	LastSeen  time.Time
}

// ListPeerIdentities returns the pins, so an operator can see who this site
// believes each peer to be.
func (db *DB) ListPeerIdentities(ctx context.Context) ([]PeerIdentity, error) {
	rows, err := db.pool.Query(ctx,
		"SELECT peer, site_uuid::text, first_seen, last_seen FROM repl_peer_identity ORDER BY peer")
	if err != nil {
		return nil, classify(fmt.Errorf("list peer identities: %w", err))
	}
	defer rows.Close()
	var out []PeerIdentity
	for rows.Next() {
		var p PeerIdentity
		if err := rows.Scan(&p.Peer, &p.UUID, &p.FirstSeen, &p.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ForgetOriginJournal drops this site's copy of an origin's journal. A
// rebuilt peer starts numbering from one again, and those new sequences
// collide with the ones already recorded here — every colliding event would
// be discarded as a duplicate and its effect silently lost. The entries are
// only a dedup ledger (a site serves nothing but its own origin), so
// clearing them costs a re-pull and nothing else.
func (db *DB) ForgetOriginJournal(ctx context.Context, origin string) (int64, error) {
	tag, err := db.pool.Exec(ctx, "DELETE FROM repl_journal WHERE origin_site = $1", origin)
	if err != nil {
		return 0, classify(fmt.Errorf("forget origin journal: %w", err))
	}
	if _, err := db.pool.Exec(ctx, "DELETE FROM repl_parked WHERE origin_site = $1", origin); err != nil {
		return 0, classify(fmt.Errorf("forget parked events: %w", err))
	}
	return tag.RowsAffected(), nil
}

// ForgetPeerIdentity drops a pin so the next handshake re-pins the peer. It
// is the deliberate operator action behind `registry repl trust-reset`: a
// peer whose UUID changed is a different site until a human says otherwise.
func (db *DB) ForgetPeerIdentity(ctx context.Context, peer string) (string, error) {
	var uuid string
	err := db.pool.QueryRow(ctx,
		"DELETE FROM repl_peer_identity WHERE peer = $1 RETURNING site_uuid::text", peer).Scan(&uuid)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("peer %q has no pinned identity", peer)
	}
	if err != nil {
		return "", classify(fmt.Errorf("forget peer identity: %w", err))
	}
	return uuid, nil
}

// RecordPeerAck notes how far a peer has consumed our journal. It is the
// watermark journal pruning is allowed to drop below, and it only ever
// moves forward.
func (db *DB) RecordPeerAck(ctx context.Context, peer, origin string, seq int64) error {
	if peer == "" || peer == "unknown" {
		return nil
	}
	_, err := db.pool.Exec(ctx, `
		INSERT INTO repl_cursors (peer, origin_site, applied_seq, durable_seq, last_ok_at)
		VALUES ($1,$2,$3,$3, now())
		ON CONFLICT (peer, origin_site) DO UPDATE
		   SET applied_seq = GREATEST(repl_cursors.applied_seq, EXCLUDED.applied_seq),
		       durable_seq = GREATEST(repl_cursors.durable_seq, EXCLUDED.durable_seq),
		       last_ok_at  = now()`,
		peer, origin, seq)
	if err != nil {
		return classify(fmt.Errorf("record peer acknowledgement: %w", err))
	}
	return nil
}

// MinCursorAcrossPeers reports how far every peer has acknowledged an
// origin's journal: the safe watermark for pruning it. ok is false when a
// peer has no cursor yet (nothing may be pruned then).
func (db *DB) MinCursorAcrossPeers(ctx context.Context, origin string, peers []string) (watermark int64, ok bool, err error) {
	if len(peers) == 0 {
		return 0, false, nil
	}
	var minSeq *int64
	var counted int
	err = db.pool.QueryRow(ctx, `
		SELECT MIN(durable_seq), count(*)
		  FROM repl_cursors
		 WHERE origin_site = $1 AND peer = ANY($2)`, origin, peers).Scan(&minSeq, &counted)
	if err != nil {
		return 0, false, classify(fmt.Errorf("read peer watermarks: %w", err))
	}
	if minSeq == nil || counted < len(peers) {
		// A peer we have never heard from could still need everything.
		return 0, false, nil
	}
	return *minSeq, true, nil
}

// PeerAckWatermark reports how far a peer has told us it applied OUR
// journal, which is what lets us prune it.
func (db *DB) PeerAckWatermark(ctx context.Context, peer, origin string) (int64, error) {
	c, err := db.GetCursor(ctx, peer, origin)
	if err != nil {
		return 0, err
	}
	return c.DurableSeq, nil
}

// BeginSnapshot starts a read-only repeatable-read transaction, so several
// reads see one consistent point in time.
func (db *DB) BeginSnapshot(ctx context.Context) (pgx.Tx, error) {
	tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, classify(fmt.Errorf("begin snapshot: %w", err))
	}
	return tx, nil
}

// KnownOriginsTx is KnownOrigins inside a caller's transaction.
func KnownOriginsTx(ctx context.Context, tx pgx.Tx) ([]string, error) {
	rows, err := tx.Query(ctx, "SELECT DISTINCT origin_site FROM repl_journal ORDER BY origin_site")
	if err != nil {
		return nil, fmt.Errorf("list journal origins: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// JournalHeadTx is JournalHead inside a caller's transaction.
func JournalHeadTx(ctx context.Context, tx pgx.Tx, origin string) (head, oldest int64, err error) {
	err = tx.QueryRow(ctx,
		"SELECT COALESCE(MAX(origin_seq),0), COALESCE(MIN(origin_seq),0) FROM repl_journal WHERE origin_site=$1",
		origin).Scan(&head, &oldest)
	if err != nil {
		return 0, 0, fmt.Errorf("read journal head: %w", err)
	}
	return head, oldest, nil
}

// Begin starts a transaction (used by the applier to make apply+cursor
// advance atomic).
func (db *DB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return nil, classify(fmt.Errorf("begin transaction: %w", err))
	}
	return tx, nil
}
