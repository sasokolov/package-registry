package adminapi

import (
	"context"
	"net/http"
	"time"

	"github.com/fondaco-dev/fondaco/core/access"
	"github.com/fondaco-dev/fondaco/core/config"
	"github.com/fondaco-dev/fondaco/core/state"
	"github.com/fondaco-dev/fondaco/core/usage"
)

// What each feed holds and how much it is used.
//
// The question an operator actually asks is "which of these is worth what it
// costs", and answering it needs both halves: a proxy feed that has cached
// 40 GB and served it 200 times is earning its keep, and one that has cached
// 40 GB and served it twice is a disk bill. Neither number means much alone,
// so they are reported together.

// FeedUsage is one feed's inventory and traffic.
type FeedUsage struct {
	Feed   string `json:"feed"`
	Format string `json:"format"`
	// Kind is "hosted", "proxy", "group" or "mixed" — what this feed is for,
	// which decides which of the numbers below are interesting.
	Kind  string `json:"kind"`
	Group bool   `json:"group,omitempty"`
	// Members is a group's members, in order.
	Members []string `json:"members,omitempty"`

	Packages  int64 `json:"packages"`
	Artifacts int64 `json:"artifacts"`
	Bytes     int64 `json:"bytes"`

	HostedPackages  int64 `json:"hosted_packages"`
	HostedArtifacts int64 `json:"hosted_artifacts"`
	HostedBytes     int64 `json:"hosted_bytes"`
	CachedPackages  int64 `json:"cached_packages"`
	CachedArtifacts int64 `json:"cached_artifacts"`
	CachedBytes     int64 `json:"cached_bytes"`
	// SharedBytes is the part of Bytes that another feed also points at.
	// Blobs are content-addressed, so deleting this feed would free
	// Bytes-SharedBytes, not Bytes.
	SharedBytes int64 `json:"shared_bytes"`

	// Downloads is how many responses this feed served, ever — across
	// replicas and restarts. Prometheus has the rate; this is the total.
	Downloads   int64 `json:"downloads"`
	BytesServed int64 `json:"bytes_served"`
	// UpstreamBytes is what was pulled in to fill the cache. Serving more
	// than was pulled is exactly what a cache is for, and the difference is
	// reported as BytesSaved.
	UpstreamBytes int64 `json:"upstream_bytes"`
	BytesSaved    int64 `json:"bytes_saved"`
	// HitRatio is the share of responses answered without asking an
	// upstream. Absent for feeds that have no upstream to ask.
	HitRatio *float64 `json:"hit_ratio,omitempty"`

	BySource map[string]SourceUsage `json:"by_source,omitempty"`

	LastIngestAt   *time.Time `json:"last_ingest_at,omitempty"`
	LastDownloadAt *time.Time `json:"last_download_at,omitempty"`
	ScannedAt      *time.Time `json:"scanned_at,omitempty"`
	// Aggregated marks numbers summed from a group's members rather than
	// measured on the feed itself.
	Aggregated bool `json:"aggregated,omitempty"`
}

// SourceUsage is one feed's counters for one response source.
type SourceUsage struct {
	Requests int64 `json:"requests"`
	Bytes    int64 `json:"bytes"`
}

// UsageTotals is the site's inventory, summed.
type UsageTotals struct {
	Feeds     int   `json:"feeds"`
	Packages  int64 `json:"packages"`
	Artifacts int64 `json:"artifacts"`
	// Bytes counts each blob once, however many feeds point at it: this is
	// what the object store actually holds, and it is smaller than the sum
	// of the per-feed numbers by exactly the sharing.
	Bytes         int64 `json:"bytes"`
	Blobs         int64 `json:"blobs"`
	Downloads     int64 `json:"downloads"`
	BytesServed   int64 `json:"bytes_served"`
	UpstreamBytes int64 `json:"upstream_bytes"`
	BytesSaved    int64 `json:"bytes_saved"`
}

// Feed kinds.
const (
	kindHosted = "hosted"
	kindProxy  = "proxy"
	kindGroup  = "group"
	kindMixed  = "mixed"
)

// handleUsage reports what every feed holds and how much it is used.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, config.SysFeeds, access.CapRead); !ok {
		return
	}
	if s.db == nil {
		s.writeError(w, http.StatusServiceUnavailable,
			"usage needs a database: the counters and the inventory are kept there")
		return
	}

	cfg := s.manager.Current()
	inventory, err := s.db.Usage(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	traffic, err := s.db.Traffic(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}

	site, err := s.db.SiteUsage(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}

	report := buildUsage(cfg, inventory, traffic)
	// The bill, as opposed to the sum of what each feed costs: a blob two
	// feeds proxy is stored once.
	report.totals.Bytes = site.DistinctBytes
	report.totals.Blobs = site.DistinctBlobs
	var scannedAt *time.Time
	if !site.ScannedAt.IsZero() {
		at := site.ScannedAt
		scannedAt = &at
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"feeds":      report.feeds,
		"totals":     report.totals,
		"scanned_at": scannedAt,
		// A site with the scan switched off reports storage as unknown
		// rather than as zero, which is a different and much worse answer.
		"scan_enabled": cfg.Server.UsageScanOrDefault() > 0,
	})
}

// TopPackage is one coordinate on the most-downloaded list.
type TopPackage struct {
	Feed       string    `json:"feed"`
	Coordinate string    `json:"coordinate"`
	Downloads  int64     `json:"downloads"`
	Bytes      int64     `json:"bytes"`
	LastAt     time.Time `json:"last_at"`
}

// handleTopPackages answers what is actually being downloaded.
//
// Feed counters say whether a feed is worth its disk; this says what in it is
// worth keeping. It is a query rather than a metric on purpose: coordinates
// are unbounded, and the only thing anyone wants from them is the top of a
// sorted list.
func (s *Server) handleTopPackages(w http.ResponseWriter, r *http.Request) {
	feed := r.URL.Query().Get("feed")
	if feed == "" {
		// Across every feed, so this is the same question as the feed list:
		// what is deployed here and how much of it is used.
		if _, ok := s.require(w, r, config.SysFeeds, access.CapRead); !ok {
			return
		}
	} else {
		// One feed's contents: whoever may list that feed may see it.
		if !s.feedExists(feed) {
			s.writeError(w, http.StatusNotFound, "no feed named "+feed)
			return
		}
		if !s.mayRead(r, feed) {
			s.writeError(w, http.StatusForbidden, "not allowed to browse this feed")
			return
		}
	}
	if s.db == nil {
		s.writeError(w, http.StatusServiceUnavailable,
			"download counts need a database: they are kept there")
		return
	}

	rows, err := s.db.TopPackages(r.Context(), feed, intParam(r, "limit", 20, 200))
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]TopPackage, 0, len(rows))
	for _, row := range rows {
		out = append(out, TopPackage{
			Feed: row.Feed, Coordinate: row.Coordinate,
			Downloads: row.Downloads, Bytes: row.Bytes, LastAt: row.LastAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"packages": out})
}

type usageReport struct {
	feeds  []FeedUsage
	totals UsageTotals
}

// buildUsage joins configuration, inventory and counters into one report.
func buildUsage(cfg *config.Config, inventory []state.FeedUsage,
	traffic []state.FeedTraffic) usageReport {
	byFeed := make(map[string]state.FeedUsage, len(inventory))
	for _, u := range inventory {
		byFeed[u.Feed] = u
	}

	type trafficSum struct {
		downloads     int64
		bytesServed   int64
		upstreamBytes int64
		fromUpstream  int64
		last          time.Time
		bySource      map[string]SourceUsage
	}
	sums := map[string]*trafficSum{}
	for _, t := range traffic {
		sum := sums[t.Feed]
		if sum == nil {
			sum = &trafficSum{bySource: map[string]SourceUsage{}}
			sums[t.Feed] = sum
		}
		if t.Source == usage.SourceIngest {
			// Not a way a request was answered: what answering cost. It is
			// deliberately not counted as activity either — a feed whose
			// cache was filled by a group's merge has been used, but saying
			// "downloaded, just now" next to a download count of zero reads
			// as a bug.
			sum.upstreamBytes += t.Bytes
			continue
		}
		if t.LastAt.After(sum.last) {
			sum.last = t.LastAt
		}
		sum.downloads += t.Requests
		sum.bytesServed += t.Bytes
		if t.Source == "upstream" {
			sum.fromUpstream += t.Requests
		}
		sum.bySource[t.Source] = SourceUsage{Requests: t.Requests, Bytes: t.Bytes}
	}

	out := make([]FeedUsage, 0, len(cfg.Feeds))
	index := map[string]int{}
	for _, f := range cfg.Feeds {
		u := FeedUsage{
			Feed: f.Name, Format: f.Format,
			Kind: feedKind(f), Group: f.IsGroup(), Members: f.Members,
		}
		if inv, ok := byFeed[f.Name]; ok {
			u.HostedPackages, u.HostedArtifacts, u.HostedBytes = inv.HostedPackages, inv.HostedArtifacts, inv.HostedBytes
			u.CachedPackages, u.CachedArtifacts, u.CachedBytes = inv.CachedPackages, inv.CachedArtifacts, inv.CachedBytes
			u.SharedBytes = inv.SharedBytes
			u.Packages, u.Artifacts, u.Bytes = inv.Packages(), inv.Artifacts(), inv.Bytes()
			if !inv.LastIngestAt.IsZero() {
				at := inv.LastIngestAt
				u.LastIngestAt = &at
			}
			at := inv.ScannedAt
			u.ScannedAt = &at
		}
		if sum := sums[f.Name]; sum != nil {
			u.Downloads, u.BytesServed, u.UpstreamBytes = sum.downloads, sum.bytesServed, sum.upstreamBytes
			u.BySource = sum.bySource
			if !sum.last.IsZero() {
				at := sum.last
				u.LastDownloadAt = &at
			}
			if f.Upstream != "" && sum.downloads > 0 {
				ratio := float64(sum.downloads-sum.fromUpstream) / float64(sum.downloads)
				u.HitRatio = &ratio
			}
		}
		u.BytesSaved = max64(u.BytesServed-u.UpstreamBytes, 0)
		index[f.Name] = len(out)
		out = append(out, u)
	}

	// A group holds nothing itself; what it is worth is what its members
	// hold and what came through its URL. Both are shown, and the stored
	// half is marked as borrowed so nobody adds it to a site total twice.
	for i, f := range cfg.Feeds {
		if !f.IsGroup() {
			continue
		}
		g := &out[i]
		for _, member := range f.Members {
			j, ok := index[member]
			if !ok {
				continue
			}
			m := out[j]
			g.Packages += m.Packages
			g.Artifacts += m.Artifacts
			g.Bytes += m.Bytes
			g.HostedPackages += m.HostedPackages
			g.HostedArtifacts += m.HostedArtifacts
			g.HostedBytes += m.HostedBytes
			g.CachedPackages += m.CachedPackages
			g.CachedArtifacts += m.CachedArtifacts
			g.CachedBytes += m.CachedBytes
		}
		g.Aggregated = g.Packages > 0 || g.Artifacts > 0
	}

	totals := UsageTotals{Feeds: len(cfg.Feeds)}
	for _, u := range out {
		if u.Group {
			// A group's content is its members', and a request it passed
			// through was already counted on the member that answered it.
			// What is only ever counted here is a document the group
			// assembled itself, so that is the part a site total adds.
			if merged, ok := u.BySource[usage.SourceMerged]; ok {
				totals.Downloads += merged.Requests
				totals.BytesServed += merged.Bytes
			}
			continue
		}
		totals.Packages += u.Packages
		totals.Artifacts += u.Artifacts
		totals.Downloads += u.Downloads
		totals.BytesServed += u.BytesServed
		totals.UpstreamBytes += u.UpstreamBytes
	}
	totals.BytesSaved = max64(totals.BytesServed-totals.UpstreamBytes, 0)

	return usageReport{feeds: out, totals: totals}
}

// feedKind names what a feed is for.
func feedKind(f config.FeedConfig) string {
	switch {
	case f.IsGroup():
		return kindGroup
	case f.Hosted && f.Upstream != "":
		return kindMixed
	case f.Hosted:
		return kindHosted
	default:
		return kindProxy
	}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// usageFor returns the compact numbers the feed list carries, so the console
// can show storage and downloads without a second request per row.
func (s *Server) usageFor(ctx context.Context) map[string]*FeedSummaryUsage {
	if s.db == nil {
		return nil
	}
	inventory, err := s.db.Usage(ctx)
	if err != nil {
		// A feed list that loses its numbers is still a feed list.
		s.logger.Debug("feed usage unavailable", "error", err)
		return nil
	}
	traffic, err := s.db.Traffic(ctx)
	if err != nil {
		s.logger.Debug("feed traffic unavailable", "error", err)
		traffic = nil
	}
	out := make(map[string]*FeedSummaryUsage, len(inventory))
	for _, u := range inventory {
		out[u.Feed] = &FeedSummaryUsage{
			Packages: u.Packages(), Artifacts: u.Artifacts(), Bytes: u.Bytes(),
		}
	}
	for _, t := range traffic {
		entry := out[t.Feed]
		if entry == nil {
			entry = &FeedSummaryUsage{}
			out[t.Feed] = entry
		}
		if t.Source == usage.SourceIngest {
			continue
		}
		entry.Downloads += t.Requests
		entry.BytesServed += t.Bytes
	}

	// A group stores nothing itself, but a list that shows it as empty
	// invites the conclusion that the group is empty. What it can serve is
	// what its members hold.
	for _, f := range s.manager.Current().Feeds {
		if !f.IsGroup() {
			continue
		}
		entry := out[f.Name]
		if entry == nil {
			entry = &FeedSummaryUsage{}
			out[f.Name] = entry
		}
		for _, member := range f.Members {
			if m := out[member]; m != nil {
				entry.Packages += m.Packages
				entry.Artifacts += m.Artifacts
				entry.Bytes += m.Bytes
			}
		}
	}
	return out
}

// FeedSummaryUsage is the short form carried by the feed list.
type FeedSummaryUsage struct {
	Packages    int64 `json:"packages"`
	Artifacts   int64 `json:"artifacts"`
	Bytes       int64 `json:"bytes"`
	Downloads   int64 `json:"downloads"`
	BytesServed int64 `json:"bytes_served"`
}
