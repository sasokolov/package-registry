package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sasokolov/package-registry/core/api"
	"github.com/sasokolov/package-registry/core/state"
)

// Publisher is the write-path counterpart of Pipeline and the ONLY place
// hosted manifests are written: format modules stage blobs and call
// CoreServices.Publish, so immutability (invariant 4), provenance, audit and
// — from Phase 7 — the replication journal live in one place.
//
// PostgreSQL is the source of truth for hosted coordinates; the manifest
// object in the blob store is a projection that keeps the read path alive
// while the database is down (invariant 7).
type Publisher struct {
	store  api.BlobStore
	db     *state.DB // nil: no database, publishing disabled
	site   string
	logger *slog.Logger
	audit  *slog.Logger
	now    func() time.Time
}

// PublisherOptions wires a Publisher.
type PublisherOptions struct {
	Store  api.BlobStore
	DB     *state.DB
	Site   string
	Logger *slog.Logger
	Audit  *slog.Logger
	Now    func() time.Time
}

// NewPublisher builds the write path.
func NewPublisher(o PublisherOptions) *Publisher {
	p := &Publisher{store: o.Store, db: o.DB, site: o.Site, logger: o.Logger, audit: o.Audit, now: o.Now}
	if p.logger == nil {
		p.logger = slog.Default()
	}
	if p.audit == nil {
		p.audit = p.logger
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.site == "" {
		p.site = "default"
	}
	return p
}

// Enabled reports whether publishing is possible (a database is required:
// immutability is enforced by a unique constraint, not by object storage).
func (p *Publisher) Enabled() bool { return p.db != nil }

// Site implements part of api.CoreServices.
func (p *Publisher) Site() string { return p.site }

// Blobs implements part of api.CoreServices.
func (p *Publisher) Blobs() api.BlobStore { return p.store }

// Publish commits one coordinate. The blob must already be staged at
// blobs/sha256/<sha256>.
func (p *Publisher) Publish(ctx context.Context, req api.PublishRequest) (api.PublishResult, error) {
	if !p.Enabled() {
		return api.PublishResult{}, fmt.Errorf("publishing requires a database: %w", api.ErrUnavailable)
	}
	if req.SHA256 == "" || req.Path == "" {
		return api.PublishResult{}, fmt.Errorf("publish needs a path and a staged blob digest: %w", api.ErrBadRequest)
	}
	if err := validRemotePath(req.Path); err != nil {
		return api.PublishResult{}, err
	}
	// The blob must exist before the coordinate becomes visible.
	if _, err := p.store.Stat(ctx, blobKey(req.SHA256)); err != nil {
		return api.PublishResult{}, fmt.Errorf("staged blob %s missing: %w", req.SHA256, err)
	}

	row := state.HostedRow{
		Feed:        req.Feed.Name,
		Path:        req.Path,
		Coordinate:  req.Coord.String(),
		SHA256:      req.SHA256,
		Size:        req.Size,
		Checksums:   req.Checksums,
		Metadata:    req.Metadata,
		Mutable:     req.Mutable,
		Origin:      "publish",
		Site:        p.site,
		PublishedBy: req.Identity.String(),
	}
	created, err := p.db.InsertHosted(ctx, row)
	if errors.Is(err, state.ErrAlreadyPublished) {
		p.audit.Warn("publish rejected: coordinate is immutable",
			"feed", req.Feed.Name, "coordinate", req.Coord.String(), "path", req.Path,
			"identity", req.Identity.String(), "site", p.site)
		return api.PublishResult{}, fmt.Errorf("%s already published: %w", req.Coord, api.ErrImmutable)
	}
	if err != nil {
		return api.PublishResult{}, err
	}

	if err := p.writeProjection(ctx, req); err != nil {
		// The coordinate is committed in the database; a missing projection
		// only costs the PG-outage fallback, so log and continue.
		p.logger.Error("hosted manifest projection failed",
			"feed", req.Feed.Name, "path", req.Path, "error", err)
	}

	p.audit.Info("package published",
		"feed", req.Feed.Name, "coordinate", req.Coord.String(), "path", req.Path,
		"sha256", req.SHA256, "size", req.Size, "identity", req.Identity.String(),
		"project_path", req.Identity.ProjectPath, "site", p.site, "created", created)
	return api.PublishResult{Created: created, SHA256: req.SHA256}, nil
}

// writeProjection mirrors the row into the blob store so the read path can
// serve hosted content without PostgreSQL.
func (p *Publisher) writeProjection(ctx context.Context, req api.PublishRequest) error {
	m := manifest{
		SHA256:     req.SHA256,
		Size:       req.Size,
		Checksums:  req.Checksums,
		IngestedAt: p.now().UTC(),
		Origin:     "publish",
		Site:       p.site,
		Publisher:  req.Identity.String(),
		Metadata:   req.Metadata,
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	key := "manifests/" + req.Feed.Name + "/" + req.Path
	if err := p.store.Put(ctx, key, bytes.NewReader(raw), api.PutOpts{}); err != nil {
		return fmt.Errorf("store manifest: %w", err)
	}
	return nil
}

// Manifests implements api.CoreServices: the deterministic input of Reindex.
func (p *Publisher) Manifests(ctx context.Context, feed api.Feed, prefix string) ([]api.HostedManifest, error) {
	if !p.Enabled() {
		return nil, fmt.Errorf("listing hosted manifests requires a database: %w", api.ErrUnavailable)
	}
	rows, err := p.db.ListHosted(ctx, feed.Name, prefix)
	if err != nil {
		return nil, err
	}
	out := make([]api.HostedManifest, 0, len(rows))
	for _, r := range rows {
		out = append(out, api.HostedManifest{
			Path:        r.Path,
			Coord:       parseCoordinate(r.Coordinate),
			SHA256:      r.SHA256,
			Size:        r.Size,
			Checksums:   r.Checksums,
			Metadata:    r.Metadata,
			Site:        r.Site,
			Publisher:   r.PublishedBy,
			PublishedAt: r.PublishedAt,
		})
	}
	return out, nil
}

// parseCoordinate reverses PackageCoordinate.String().
func parseCoordinate(s string) api.PackageCoordinate {
	format, rest, ok := cut(s, ":")
	if !ok {
		return api.PackageCoordinate{Name: s}
	}
	name, version, hasVersion := lastCut(rest, "@")
	if !hasVersion {
		return api.PackageCoordinate{Format: format, Name: rest}
	}
	return api.PackageCoordinate{Format: format, Name: name, Version: version}
}

func cut(s, sep string) (before, after string, found bool) {
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):], true
		}
	}
	return s, "", false
}

func lastCut(s, sep string) (before, after string, found bool) {
	for i := len(s) - len(sep); i >= 0; i-- {
		if s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):], true
		}
	}
	return s, "", false
}

// PutIndex implements api.CoreServices: feed indexes are derived data,
// stored as ordinary local objects and never replicated (invariant 15).
func (p *Publisher) PutIndex(ctx context.Context, feed api.Feed, path string, body []byte) error {
	if err := validRemotePath(path); err != nil {
		return err
	}
	key := "meta/" + feed.Name + "/" + path
	if err := p.store.Put(ctx, key, bytes.NewReader(body), api.PutOpts{}); err != nil {
		return fmt.Errorf("store index %s: %w", path, err)
	}
	return nil
}

// Reindex regenerates a hosted feed's indexes under the module's control,
// serialized across replicas by an advisory lock so two concurrent
// publishes cannot interleave index writes.
func (p *Publisher) Reindex(ctx context.Context, feed api.Feed, module api.FormatModule) error {
	hoster, ok := module.(api.Hoster)
	if !ok {
		return nil
	}
	run := func(ctx context.Context) error { return hoster.Reindex(ctx, feed, p) }
	if p.db == nil {
		return run(ctx)
	}
	err := p.db.WithLock(ctx, "reindex/"+feed.Name, run)
	if err != nil && errors.Is(err, state.ErrLockUnavailable) {
		p.logger.Warn("reindex without cross-replica lock (lock backend down)",
			"feed", feed.Name, "error", err)
		return run(ctx)
	}
	return err
}
