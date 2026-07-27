package adminapi

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// The read surface the console needs. It answers questions an operator
// actually asks — what is configured, what is stored, is replication
// healthy, what is blocked and why — and it never returns a secret
// (invariant 12): tokens appear by name and hash prefix only.

// SiteStatus is the overview response.
type SiteStatus struct {
	Site          string `json:"site"`
	ConfigVersion string `json:"config_version,omitempty"`
	ConfigSource  string `json:"config_source,omitempty"`
	Feeds         int    `json:"feeds"`
	Database      string `json:"database,omitempty"`
	Replication   struct {
		Enabled  bool   `json:"enabled"`
		Peers    int    `json:"peers"`
		Topology string `json:"topology,omitempty"`
	} `json:"replication"`
}

// FeedSummary describes one feed for a list view.
type FeedSummary struct {
	Name            string   `json:"name"`
	Format          string   `json:"format"`
	Upstream        string   `json:"upstream,omitempty"`
	Hosted          bool     `json:"hosted"`
	Anonymous       bool     `json:"anonymous"`
	Publishers      []string `json:"publishers,omitempty"`
	Policies        []string `json:"policies,omitempty"`
	PublishPolicy   string   `json:"publish_policy,omitempty"`
	ReplicationMode string   `json:"replication_mode,omitempty"`
	PeerFallback    bool     `json:"peer_fallback,omitempty"`
	// Packages is absent rather than zero when the caller may not be told:
	// "none" and "not your business" are different answers.
	Packages *int `json:"packages,omitempty"`
}

// handleStatus answers the overview.
//
// An anonymous caller gets the site name and how many feeds are open to it,
// and nothing else. The rest — which document this site is running, where it
// is kept, whether the database is up, who its peers are — is operational
// detail about the deployment, and a stranger downloading a public package
// has no business reading it.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	cfg := s.manager.Current()
	// The site name is on every response as X-Registry-Source's companion
	// header anyway, so hiding it here would only be theatre.
	out := SiteStatus{Site: cfg.Site.Name}

	if s.identity(r).IsAnonymous() {
		for _, f := range cfg.Feeds {
			if f.Anonymous {
				out.Feeds++
			}
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	out.ConfigVersion = s.manager.Version()
	out.ConfigSource = s.manager.Source().Describe()
	out.Feeds = len(cfg.Feeds)
	out.Database = "disabled"
	if s.db != nil {
		out.Database = "up"
		if err := s.db.Ping(r.Context()); err != nil {
			// A database outage degrades rather than fails (invariant 7),
			// and the console should say which of the two it is looking at.
			out.Database = "unavailable"
		}
	}
	out.Replication.Enabled = cfg.Replication.Enabled
	out.Replication.Peers = len(cfg.Replication.Peers)
	out.Replication.Topology = cfg.Replication.Topology
	writeJSON(w, http.StatusOK, out)
}

// WhoAmI tells the console who it is talking as and what it may do, so the
// UI can hide actions instead of offering ones that will be refused.
type WhoAmI struct {
	Kind        string   `json:"kind"`
	Subject     string   `json:"subject"`
	ProjectPath string   `json:"project_path,omitempty"`
	Admin       bool     `json:"admin"`
	Stale       bool     `json:"stale,omitempty"`
	CanPublish  []string `json:"can_publish,omitempty"`
	// AuthError is set when the caller offered a credential that was not
	// accepted. Without it a rejected token is indistinguishable from no
	// token at all, and a client has no way to tell "I am browsing
	// anonymously" from "what I pasted is wrong".
	AuthError string `json:"auth_error,omitempty"`
}

func (s *Server) handleWhoAmI(w http.ResponseWriter, r *http.Request) {
	id, refusal := s.identityOrRefusal(r)
	out := WhoAmI{
		Kind:        string(id.Kind),
		Subject:     id.Subject,
		ProjectPath: id.ProjectPath,
		Admin:       s.isAdmin(id),
		Stale:       id.Stale,
		AuthError:   refusal,
	}
	if s.deps.CanPublish != nil {
		out.CanPublish = s.deps.CanPublish(id)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleFeeds lists feeds with what the console shows in a table.
func (s *Server) handleFeeds(w http.ResponseWriter, r *http.Request) {
	if s.deps.FeedSummaries != nil {
		writeJSON(w, http.StatusOK, map[string]any{"feeds": s.deps.FeedSummaries(r.Context())})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"feeds": s.feedSummaries(r)})
}

// feedSummaries builds the list from configuration and stored counts.
//
// What an anonymous caller sees is only the feeds it may actually read, and
// only the facts it could work out by using them: the name, the format and
// whether they are hosted. Where a feed proxies from, who may publish to it,
// which policies guard it and how much it holds are configuration, and
// listing them for strangers would hand out a map of the deployment —
// including the existence of feeds they cannot open.
func (s *Server) feedSummaries(r *http.Request) []FeedSummary {
	cfg := s.manager.Current()
	anonymous := s.identity(r).IsAnonymous()

	counts := map[string]int{}
	if s.db != nil && !anonymous {
		if rows, err := s.db.ListHosted(r.Context(), "", ""); err == nil {
			for _, row := range rows {
				counts[row.Feed]++
			}
		}
	}

	out := make([]FeedSummary, 0, len(cfg.Feeds))
	for _, f := range cfg.Feeds {
		if anonymous {
			if !f.Anonymous {
				continue
			}
			out = append(out, FeedSummary{
				Name: f.Name, Format: f.Format,
				Hosted: f.Hosted, Anonymous: true,
			})
			continue
		}
		summary := FeedSummary{
			Name: f.Name, Format: f.Format, Upstream: f.Upstream,
			Hosted: f.Hosted, Anonymous: f.Anonymous,
			Publishers: f.Publishers, PublishPolicy: f.PublishPolicy,
			ReplicationMode: f.ReplicationMode, PeerFallback: f.PeerFallback,
		}
		count := counts[f.Name]
		summary.Packages = &count
		for _, p := range f.Policies {
			summary.Policies = append(summary.Policies, p.Name)
		}
		out = append(out, summary)
	}
	return out
}

// PackageEntry is one stored coordinate.
type PackageEntry struct {
	Feed        string            `json:"feed"`
	Path        string            `json:"path"`
	Coordinate  string            `json:"coordinate"`
	SHA256      string            `json:"sha256"`
	Size        int64             `json:"size"`
	Checksums   map[string]string `json:"checksums,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Site        string            `json:"site,omitempty"`
	PublishedBy string            `json:"published_by,omitempty"`
	PublishedAt time.Time         `json:"published_at"`
	Quarantined bool              `json:"quarantined,omitempty"`
}

// handlePackages lists a feed's stored coordinates, filtered by a substring
// and paginated by offset. The registry has no search index and inventing
// one here would be a second source of truth; this is a scan over the rows
// the feed already has, which is what a console needs and what an operator
// expects to be exact.
func (s *Server) handlePackages(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		s.writeError(w, http.StatusServiceUnavailable, "package listing needs a database")
		return
	}
	feed := chi.URLParam(r, "feed")
	if !s.feedExists(feed) {
		s.writeError(w, http.StatusNotFound, "no feed named "+feed)
		return
	}
	if !s.mayRead(r, feed) {
		s.writeError(w, http.StatusForbidden, "not allowed to browse this feed")
		return
	}

	query := strings.ToLower(r.URL.Query().Get("q"))
	limit := intParam(r, "limit", 100, 1000)
	offset := intParam(r, "offset", 0, 1<<20)

	rows, err := s.db.ListHosted(r.Context(), feed, "")
	if err != nil {
		s.fail(w, err)
		return
	}

	var matched []PackageEntry
	for _, row := range rows {
		if query != "" &&
			!strings.Contains(strings.ToLower(row.Path), query) &&
			!strings.Contains(strings.ToLower(row.Coordinate), query) {
			continue
		}
		matched = append(matched, PackageEntry{
			Feed: row.Feed, Path: row.Path, Coordinate: row.Coordinate,
			SHA256: row.SHA256, Size: row.Size,
			Checksums: row.Checksums, Metadata: row.Metadata,
			Site: row.Site, PublishedBy: row.PublishedBy, PublishedAt: row.PublishedAt,
		})
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].Path < matched[j].Path })

	total := len(matched)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"feed":     feed,
		"total":    total,
		"offset":   offset,
		"limit":    limit,
		"packages": matched[offset:end],
	})
}

// handlePackage returns one coordinate in full, including whether it is
// blocked and why.
func (s *Server) handlePackage(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		s.writeError(w, http.StatusServiceUnavailable, "package details need a database")
		return
	}
	feed := chi.URLParam(r, "feed")
	path := chi.URLParam(r, "*")
	if !s.feedExists(feed) {
		s.writeError(w, http.StatusNotFound, "no feed named "+feed)
		return
	}
	if !s.mayRead(r, feed) {
		s.writeError(w, http.StatusForbidden, "not allowed to browse this feed")
		return
	}

	row, found, err := s.db.HostedRow(r.Context(), feed, path)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !found {
		s.writeError(w, http.StatusNotFound, "no such coordinate")
		return
	}

	entry := PackageEntry{
		Feed: row.Feed, Path: row.Path, Coordinate: row.Coordinate,
		SHA256: row.SHA256, Size: row.Size,
		Checksums: row.Checksums, Metadata: row.Metadata,
		Site: row.Site, PublishedBy: row.PublishedBy, PublishedAt: row.PublishedAt,
	}
	if _, active, err := s.db.ActiveQuarantine(r.Context(), feed, row.Coordinate); err == nil {
		entry.Quarantined = active
	}
	writeJSON(w, http.StatusOK, entry)
}

// feedExists reports whether a feed is configured.
func (s *Server) feedExists(name string) bool {
	for _, f := range s.manager.Current().Feeds {
		if f.Name == name {
			return true
		}
	}
	return false
}

// mayRead reports whether the caller may browse a feed: anonymous feeds are
// open, the rest need an identity, exactly as the download path decides.
func (s *Server) mayRead(r *http.Request, feed string) bool {
	for _, f := range s.manager.Current().Feeds {
		if f.Name != feed {
			continue
		}
		if f.Anonymous {
			return true
		}
		return !s.identity(r).IsAnonymous()
	}
	return false
}

func intParam(r *http.Request, name string, def, upperBound int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return def
	}
	if v > upperBound {
		return upperBound
	}
	return v
}
