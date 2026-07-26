package repl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/sasokolov/package-registry/core/api"
	"github.com/sasokolov/package-registry/core/state"
)

// journalBatch bounds one poll.
const journalBatch = 200

// Manager runs the pull loops: for every peer, for every origin that peer
// knows, apply what we are missing and advance the cursor.
type Manager struct {
	db        *state.DB
	store     api.BlobStore
	site      string
	clients   []*Client
	applier   *Applier
	logger    *slog.Logger
	metrics   *Metrics
	digests   func(ctx context.Context) map[string]string
	retention time.Duration

	// missing is a negative cache for peer lookups: a coordinate no peer
	// has must not trigger a fan-out on every request.
	missingMu sync.Mutex
	missing   map[string]time.Time
	missTTL   time.Duration

	// peers is swapped wholesale on config reload; running loops for peers
	// that disappeared stop on their own next tick.
	peersMu sync.RWMutex
	peers   map[string]*Client
	// running tracks which peers already have a poll loop.
	running map[string]context.CancelFunc
}

// ManagerOptions wires the replication manager.
type ManagerOptions struct {
	DB        *state.DB
	Store     api.BlobStore
	Site      string
	Clients   []*Client
	Applier   *Applier
	Logger    *slog.Logger
	Metrics   *Metrics
	Digests   func(ctx context.Context) map[string]string
	Retention time.Duration
	// MissTTL bounds the negative cache of peer lookups (default 10s).
	MissTTL time.Duration
}

// NewManager builds the manager.
func NewManager(o ManagerOptions) *Manager {
	m := &Manager{
		db: o.DB, store: o.Store, site: o.Site, clients: o.Clients,
		applier: o.Applier, logger: o.Logger, metrics: o.Metrics,
		digests: o.Digests, retention: o.Retention,
		missing: map[string]time.Time{}, missTTL: o.MissTTL,
		peers: map[string]*Client{}, running: map[string]context.CancelFunc{},
	}
	for _, c := range o.Clients {
		m.peers[c.Name()] = c
	}
	if m.missTTL <= 0 {
		m.missTTL = 10 * time.Second
	}
	if m.logger == nil {
		m.logger = slog.Default()
	}
	return m
}

// clientList snapshots the current peer set.
func (m *Manager) clientList() []*Client {
	m.peersMu.RLock()
	defer m.peersMu.RUnlock()
	out := make([]*Client, 0, len(m.peers))
	for _, c := range m.peers {
		out = append(out, c)
	}
	return out
}

// Run polls every peer until ctx is done. Each peer has its own loop, so a
// slow or unreachable peer never blocks the others; loops start and stop as
// the configured peer set changes.
func (m *Manager) Run(ctx context.Context) {
	m.syncLoops(ctx)
	<-ctx.Done()

	m.peersMu.Lock()
	for _, cancel := range m.running {
		cancel()
	}
	m.running = map[string]context.CancelFunc{}
	m.peersMu.Unlock()
}

// syncLoops starts a poll loop for every peer that lacks one and stops the
// loops of peers that were removed from the config.
func (m *Manager) syncLoops(ctx context.Context) {
	m.peersMu.Lock()
	defer m.peersMu.Unlock()
	for name, c := range m.peers {
		if _, ok := m.running[name]; ok {
			continue
		}
		peerCtx, cancel := context.WithCancel(ctx)
		m.running[name] = cancel
		go m.runPeer(peerCtx, c)
		m.logger.Info("replication peer loop started", "peer", name)
	}
	for name, cancel := range m.running {
		if _, ok := m.peers[name]; ok {
			continue
		}
		cancel()
		delete(m.running, name)
		m.logger.Info("replication peer loop stopped (removed from config)", "peer", name)
	}
}

// SetPeers replaces the peer set after a config reload. Peers that are
// unchanged keep their client (and its pinned site UUID), so a reload never
// re-opens the trust decision.
func (m *Manager) SetPeers(ctx context.Context, clients []*Client) {
	m.peersMu.Lock()
	next := make(map[string]*Client, len(clients))
	for _, c := range clients {
		if existing, ok := m.peers[c.Name()]; ok && existing.SameEndpoint(c) {
			next[c.Name()] = existing
			continue
		}
		next[c.Name()] = c
	}
	m.peers = next
	m.peersMu.Unlock()
	m.syncLoops(ctx)
}

func (m *Manager) runPeer(ctx context.Context, c *Client) {
	ticker := time.NewTicker(c.Interval())
	defer ticker.Stop()

	// Poll immediately so a fresh site converges without waiting an interval.
	m.pollOnce(ctx, c)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.pollOnce(ctx, c)
		}
	}
}

// pollOnce runs a poll cycle under a per-peer lease, so the N stateless
// replicas of a site do not all pull the same stream (invariant 9: the
// coordination primitive is a PostgreSQL advisory lock, not Redis).
func (m *Manager) pollOnce(ctx context.Context, c *Client) {
	ran, err := m.db.TryLease(ctx, "repl-poll:"+m.site+":"+c.Name(), func(ctx context.Context) error {
		m.pollPeer(ctx, c)
		return nil
	})
	if err != nil {
		// The lease backend is down: poll anyway. Apply is idempotent, so
		// a duplicated poll costs traffic, not correctness (invariant 7).
		m.logger.Warn("replication lease unavailable, polling without it", "peer", c.Name(), "error", err)
		m.pollPeer(ctx, c)
		return
	}
	if !ran {
		m.logger.Debug("another replica is polling this peer", "peer", c.Name())
	}
}

// pollPeer runs one poll cycle: handshake, per-origin catch-up, parked
// retries, and metric updates.
func (m *Manager) pollPeer(ctx context.Context, c *Client) {
	status, err := c.Status(ctx)
	if err != nil {
		m.metrics.pollFailure(c.Name())
		m.logger.Warn("peer poll failed", "peer", c.Name(), "error", err)
		_ = m.db.RecordCursorError(ctx, c.Name(), "*", err.Error())
		return
	}

	// A reachable peer is recorded even when there is nothing to apply, so
	// an idle healthy stream is distinguishable from an unreachable one.
	origins := make([]string, 0, len(status.Heads))
	for origin := range status.Heads {
		if origin != m.site {
			origins = append(origins, origin)
		}
	}
	if err := m.db.MarkPeerPollOK(ctx, c.Name(), origins); err != nil {
		m.logger.Warn("recording a successful poll failed", "peer", c.Name(), "error", err)
	}

	touched := map[string]bool{}
	for origin, head := range status.Heads {
		if origin == m.site {
			// Our own events echoed by the peer: nothing to import.
			continue
		}
		applied, err := m.catchUp(ctx, c, origin, head, touched)
		if err != nil {
			m.metrics.pollFailure(c.Name())
			m.logger.Warn("peer stream catch-up failed",
				"peer", c.Name(), "origin", origin, "error", err)
			_ = m.db.RecordCursorError(ctx, c.Name(), origin, err.Error())
			continue
		}
		if applied > 0 {
			m.logger.Info("replication applied",
				"peer", c.Name(), "origin", origin, "events", applied)
		}
		m.updateLag(ctx, c.Name(), origin, head)
	}

	m.pruneJournal(ctx, c.Name())

	if err := m.applier.RetryParked(ctx, c.Name()); err != nil {
		m.logger.Warn("retrying parked events failed", "peer", c.Name(), "error", err)
	}
	if err := m.applier.ReindexTouched(ctx, touched); err != nil {
		m.logger.Warn("reindex after replication failed", "error", err)
	}
	m.updateObservability(ctx, c.Name())
}

// catchUp pages through one origin's journal until we reach its head.
func (m *Manager) catchUp(ctx context.Context, c *Client, origin string, head int64, touched map[string]bool) (int, error) {
	var appliedTotal int
	for {
		cursor, err := m.db.GetCursor(ctx, c.Name(), origin)
		if err != nil {
			return appliedTotal, err
		}
		if cursor.AppliedSeq >= head {
			return appliedTotal, nil
		}

		page, err := c.Journal(ctx, origin, cursor.AppliedSeq, journalBatch)
		if errors.Is(err, ErrResync) {
			m.logger.Warn("cursor fell behind peer retention, bootstrapping from snapshot",
				"peer", c.Name(), "origin", origin, "cursor", cursor.AppliedSeq)
			if err := m.bootstrap(ctx, c, touched); err != nil {
				return appliedTotal, err
			}
			return appliedTotal, nil
		}
		if err != nil {
			return appliedTotal, err
		}
		if len(page.Entries) == 0 {
			return appliedTotal, nil
		}

		batchTouched, err := m.applier.ApplyBatch(ctx, c.Name(), page.Entries)
		for feed := range batchTouched {
			touched[feed] = true
		}
		if err != nil {
			return appliedTotal, err
		}

		last := page.Entries[len(page.Entries)-1].OriginSeq
		durable := m.durableThrough(ctx, page.Entries, last)
		if err := m.advanceCursor(ctx, c.Name(), origin, last, durable); err != nil {
			return appliedTotal, err
		}
		appliedTotal += len(page.Entries)
	}
}

// durableThrough reports the highest sequence whose referenced blobs are
// all present locally — the honest RPO of this site.
func (m *Manager) durableThrough(ctx context.Context, entries []state.JournalEntry, fallback int64) int64 {
	durable := fallback
	for _, e := range entries {
		if e.Kind != KindManifestPut {
			continue
		}
		var p ManifestPut
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		if p.SHA256 == "" {
			continue
		}
		if _, err := m.store.Stat(ctx, "blobs/sha256/"+p.SHA256); err != nil {
			// This event's bytes are not here yet: durability stops before it.
			if e.OriginSeq-1 < durable {
				durable = e.OriginSeq - 1
			}
		}
	}
	if durable < 0 {
		return 0
	}
	return durable
}

func (m *Manager) advanceCursor(ctx context.Context, peer, origin string, applied, durable int64) error {
	tx, err := m.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := state.SetCursorTx(ctx, tx, state.Cursor{
		Peer: peer, Origin: origin, AppliedSeq: applied, DurableSeq: durable,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// bootstrap imports a peer's full state, then sets cursors to the
// watermarks the snapshot corresponds to.
func (m *Manager) bootstrap(ctx context.Context, c *Client, touched map[string]bool) error {
	snap, err := c.Snapshot(ctx)
	if err != nil {
		return err
	}
	m.logger.Info("bootstrapping from peer snapshot",
		"peer", c.Name(), "manifests", len(snap.Manifests))

	for _, p := range snap.Manifests {
		entry := state.JournalEntry{
			OriginSite:    snap.Site,
			Kind:          KindManifestPut,
			SchemaVersion: SchemaVersion,
		}
		payload, err := json.Marshal(p)
		if err != nil {
			return err
		}
		entry.Payload = payload
		tx, err := m.db.Begin(ctx)
		if err != nil {
			return err
		}
		if err := m.applier.dispatch(ctx, tx, c.Name(), entry, touched); err != nil {
			_ = tx.Rollback(ctx)
			if errors.Is(err, errPark) {
				// Missing blob during bootstrap: park and retry later, the
				// backfill will bring it.
				_ = m.db.ParkEvent(ctx, entry, err.Error())
				continue
			}
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}

	for origin, seq := range snap.Watermarks {
		if origin == m.site {
			continue
		}
		if err := m.advanceCursor(ctx, c.Name(), origin, seq, 0); err != nil {
			return err
		}
	}
	return nil
}

// pruneJournal drops our own journal entries every peer has durably
// applied, bounded by the configured retention: a peer that has not
// acknowledged yet always keeps its history.
func (m *Manager) pruneJournal(ctx context.Context, peer string) {
	if m.retention <= 0 {
		return
	}
	names := make([]string, 0, len(m.peers))
	for _, c := range m.clientList() {
		names = append(names, c.Name())
	}
	watermark, ok, err := m.db.MinCursorAcrossPeers(ctx, m.site, names)
	if err != nil || !ok || watermark <= 0 {
		return
	}
	n, err := m.db.PruneJournal(ctx, m.site, watermark+1)
	if err != nil {
		m.logger.Warn("journal prune failed", "error", err)
		return
	}
	if n > 0 {
		m.logger.Info("journal pruned",
			"site", m.site, "below_seq", watermark+1, "entries", n, "acked_by", peer)
	}
}

// updateLag publishes the per-stream lag gauges.
func (m *Manager) updateLag(ctx context.Context, peer, origin string, head int64) {
	if m.metrics == nil {
		return
	}
	cursor, err := m.db.GetCursor(ctx, peer, origin)
	if err != nil {
		return
	}
	m.metrics.Lag.WithLabelValues(peer, origin).Set(float64(head - cursor.AppliedSeq))
	m.metrics.DurableLag.WithLabelValues(peer, origin).Set(float64(head - cursor.DurableSeq))
	if !cursor.LastOKAt.IsZero() {
		m.metrics.CursorAge.WithLabelValues(peer).Set(time.Since(cursor.LastOKAt).Seconds())
	}
}

// updateObservability refreshes parked counts and per-feed digests, which
// is how divergence becomes visible instead of silent (invariant 16).
func (m *Manager) updateObservability(ctx context.Context, peer string) {
	if m.metrics == nil {
		return
	}
	if n, err := m.db.CountParked(ctx); err == nil {
		m.metrics.Parked.Set(float64(n))
	}
	if m.digests == nil {
		return
	}
	for feed, digest := range m.digests(ctx) {
		m.metrics.FeedDigest.WithLabelValues(feed).Set(digestToFloat(digest))
	}
	_ = peer
}

// digestToFloat turns a hex digest into a comparable gauge value: equal
// digests give equal numbers, so a dashboard can diff sites directly.
func digestToFloat(digest string) float64 {
	var v uint64
	for i := 0; i < len(digest) && i < 13; i++ {
		v = v*16 + uint64(hexVal(digest[i]))
	}
	return float64(v)
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return 0
	}
}

// EnsureBlob implements BlobFetcher for the applier: fetch a blob from the
// named peer (or any peer that has it).
func (m *Manager) EnsureBlob(ctx context.Context, sha256hex string, size int64, fromPeer string) error {
	if m.HasBlob(ctx, sha256hex) {
		return nil
	}
	clients := m.clientList()
	ordered := make([]*Client, 0, len(clients))
	for _, c := range clients {
		if c.Name() == fromPeer {
			ordered = append([]*Client{c}, ordered...)
			continue
		}
		ordered = append(ordered, c)
	}
	var lastErr error
	for _, c := range ordered {
		if err := c.FetchBlob(ctx, m.store, sha256hex, size); err != nil {
			lastErr = err
			continue
		}
		m.metrics.peerFetch("ok")
		return nil
	}
	m.metrics.peerFetch("failed")
	if lastErr == nil {
		lastErr = fmt.Errorf("no peer has blob %s: %w", short(sha256hex), api.ErrNotFound)
	}
	return lastErr
}

// HasBlob implements BlobFetcher.
func (m *Manager) HasBlob(ctx context.Context, sha256hex string) bool {
	_, err := m.store.Stat(ctx, "blobs/sha256/"+sha256hex)
	return err == nil
}

// FetchManifest implements pipeline.PeerSource: ask peers for a hosted
// coordinate this site does not have yet.
func (m *Manager) FetchManifest(ctx context.Context, feed api.Feed, path string) (string, int64, error) {
	key := feed.Name + "/" + path
	if m.recentlyMissing(key) {
		return "", 0, api.ErrNotFound
	}
	var lastErr = api.ErrNotFound
	for _, c := range m.clientList() {
		res, err := c.Manifest(ctx, feed.Name, path)
		if err != nil {
			lastErr = err
			continue
		}
		return res.SHA256, res.Size, nil
	}
	// Remember the miss briefly: a 404 storm must not fan out to every peer
	// on every request.
	m.noteMissing(key)
	return "", 0, lastErr
}

func (m *Manager) recentlyMissing(key string) bool {
	m.missingMu.Lock()
	defer m.missingMu.Unlock()
	until, ok := m.missing[key]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(m.missing, key)
		return false
	}
	return true
}

func (m *Manager) noteMissing(key string) {
	m.missingMu.Lock()
	defer m.missingMu.Unlock()
	// Bound the map: this is a cache, not a ledger.
	if len(m.missing) > 4096 {
		m.missing = map[string]time.Time{}
	}
	m.missing[key] = time.Now().Add(m.missTTL)
}

// ForwardPublish sends a write to the named home site over the replication
// channel.
func (m *Manager) ForwardPublish(ctx context.Context, site, feed, path, method string,
	body io.Reader, identity, projectPath string) (int, []byte, error) {
	for _, c := range m.clientList() {
		if c.Name() == site {
			return c.ForwardPublish(ctx, feed, path, method, body, identity, projectPath)
		}
	}
	return 0, nil, fmt.Errorf("no peer configured for home site %q", site)
}

// NudgePeers tells every peer that new events exist (best effort).
func (m *Manager) NudgePeers(ctx context.Context) {
	for _, c := range m.clientList() {
		go c.Nudge(ctx)
	}
}
