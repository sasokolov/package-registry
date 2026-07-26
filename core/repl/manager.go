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
	db      *state.DB
	store   api.BlobStore
	site    string
	clients []*Client
	applier *Applier
	logger  *slog.Logger
	metrics *Metrics
	digests func(ctx context.Context) map[string]string
}

// ManagerOptions wires the replication manager.
type ManagerOptions struct {
	DB      *state.DB
	Store   api.BlobStore
	Site    string
	Clients []*Client
	Applier *Applier
	Logger  *slog.Logger
	Metrics *Metrics
	Digests func(ctx context.Context) map[string]string
}

// NewManager builds the manager.
func NewManager(o ManagerOptions) *Manager {
	m := &Manager{
		db: o.DB, store: o.Store, site: o.Site, clients: o.Clients,
		applier: o.Applier, logger: o.Logger, metrics: o.Metrics,
		digests: o.Digests,
	}
	if m.logger == nil {
		m.logger = slog.Default()
	}
	return m
}

// Run polls every peer until ctx is done. Each peer has its own loop, so a
// slow or unreachable peer never blocks the others.
func (m *Manager) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, c := range m.clients {
		wg.Add(1)
		go func(c *Client) {
			defer wg.Done()
			m.runPeer(ctx, c)
		}(c)
	}
	wg.Wait()
}

func (m *Manager) runPeer(ctx context.Context, c *Client) {
	ticker := time.NewTicker(c.Interval())
	defer ticker.Stop()

	// Poll immediately so a fresh site converges without waiting an interval.
	m.pollPeer(ctx, c)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.pollPeer(ctx, c)
		}
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
	ordered := make([]*Client, 0, len(m.clients))
	for _, c := range m.clients {
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
	var lastErr = api.ErrNotFound
	for _, c := range m.clients {
		res, err := c.Manifest(ctx, feed.Name, path)
		if err != nil {
			lastErr = err
			continue
		}
		return res.SHA256, res.Size, nil
	}
	return "", 0, lastErr
}

// ForwardPublish sends a write to the named home site over the replication
// channel.
func (m *Manager) ForwardPublish(ctx context.Context, site, feed, path, method string,
	body io.Reader, identity, projectPath string) (int, []byte, error) {
	for _, c := range m.clients {
		if c.Name() == site {
			return c.ForwardPublish(ctx, feed, path, method, body, identity, projectPath)
		}
	}
	return 0, nil, fmt.Errorf("no peer configured for home site %q", site)
}

// NudgePeers tells every peer that new events exist (best effort).
func (m *Manager) NudgePeers(ctx context.Context) {
	for _, c := range m.clients {
		go c.Nudge(ctx)
	}
}
