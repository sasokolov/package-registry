package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/fondaco-dev/fondaco/core/api"
	"github.com/fondaco-dev/fondaco/core/state"
	"github.com/fondaco-dev/fondaco/modules/storage/fs"
)

// The scan runs against the real filesystem store rather than a fake: what
// it is doing is walking a store, and a fake that lists the way the scanner
// expects would prove nothing.
func newStore(t *testing.T) api.BlobStore {
	t.Helper()
	store, err := fs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func writeManifest(t *testing.T, store api.BlobStore, key string, m storedManifest) {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), key, bytes.NewReader(raw), api.PutOpts{}); err != nil {
		t.Fatal(err)
	}
}

func scan(t *testing.T, store api.BlobStore, feeds ...Feed) Report {
	t.Helper()
	s := NewScanner(Options{
		Store: store,
		Feeds: func() []Feed { return feeds },
	})
	report, err := s.ScanOnce(t.Context())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	return report
}

func find(t *testing.T, report Report, feed string) state.FeedUsage {
	t.Helper()
	for _, u := range report.Feeds {
		if u.Feed == feed {
			return u
		}
	}
	t.Fatalf("no row for %s in %+v", feed, report.Feeds)
	return state.FeedUsage{}
}

// A proxy feed is a cache, and what it has cached is only knowable from the
// store: there are no rows for it anywhere, on purpose.
func TestAProxyFeedIsCountedFromWhatItCached(t *testing.T) {
	store := newStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	writeManifest(t, store, "manifests/npmjs/pkg/-/pkg-1.0.0.tgz", storedManifest{
		SHA256: "aaa", Size: 100, Coordinate: "npm:pkg@1.0.0", Origin: "proxy", IngestedAt: now.Add(-time.Hour),
	})
	writeManifest(t, store, "manifests/npmjs/pkg/-/pkg-1.1.0.tgz", storedManifest{
		SHA256: "bbb", Size: 200, Coordinate: "npm:pkg@1.1.0", Origin: "proxy", IngestedAt: now,
	})
	// Two files of one version: artifacts and packages are different
	// questions, and an operator asking "how many packages" means the
	// second.
	writeManifest(t, store, "manifests/npmjs/pkg/-/pkg-1.1.0.tgz.sig", storedManifest{
		SHA256: "ccc", Size: 10, Coordinate: "npm:pkg@1.1.0", Origin: "proxy", IngestedAt: now,
	})

	u := find(t, scan(t, store, Feed{Name: "npmjs", Format: "npm"}), "npmjs")
	if u.CachedArtifacts != 3 {
		t.Errorf("artifacts = %d, want 3", u.CachedArtifacts)
	}
	if u.CachedPackages != 2 {
		t.Errorf("packages = %d, want 2 distinct coordinates", u.CachedPackages)
	}
	if u.CachedBytes != 310 {
		t.Errorf("bytes = %d, want 310", u.CachedBytes)
	}
	if u.HostedArtifacts != 0 || u.HostedBytes != 0 {
		t.Errorf("a proxy feed reported hosted content: %+v", u)
	}
	if !u.LastIngestAt.Equal(now) {
		t.Errorf("last ingest = %v, want the newest manifest's %v", u.LastIngestAt, now)
	}
}

// Blobs are content-addressed, so two feeds proxying the same tarball store
// it once. Both feeds cost what it costs, but deleting either frees nothing:
// the difference is what shared_bytes is for, and what stops a site total
// from being the sum of its feeds.
func TestSharedBlobsAreCountedForEachFeedAndOnceForTheSite(t *testing.T) {
	store := newStore(t)
	writeManifest(t, store, "manifests/npmjs/pkg/-/pkg-1.0.0.tgz", storedManifest{
		SHA256: "shared", Size: 100, Coordinate: "npm:pkg@1.0.0", Origin: "proxy",
	})
	writeManifest(t, store, "manifests/npm-mirror/pkg/-/pkg-1.0.0.tgz", storedManifest{
		SHA256: "shared", Size: 100, Coordinate: "npm:pkg@1.0.0", Origin: "proxy",
	})
	writeManifest(t, store, "manifests/npmjs/other/-/other-1.0.0.tgz", storedManifest{
		SHA256: "alone", Size: 50, Coordinate: "npm:other@1.0.0", Origin: "proxy",
	})

	report := scan(t, store,
		Feed{Name: "npmjs", Format: "npm"}, Feed{Name: "npm-mirror", Format: "npm"})

	first := find(t, report, "npmjs")
	second := find(t, report, "npm-mirror")
	if first.Bytes() != 150 || second.Bytes() != 100 {
		t.Errorf("per-feed bytes = %d and %d, want 150 and 100", first.Bytes(), second.Bytes())
	}
	if first.SharedBytes != 100 || second.SharedBytes != 100 {
		t.Errorf("shared = %d and %d, want 100 each", first.SharedBytes, second.SharedBytes)
	}
	if report.Site.DistinctBytes != 150 {
		t.Errorf("site bytes = %d, want 150 — the store holds the shared blob once",
			report.Site.DistinctBytes)
	}
	if report.Site.DistinctBlobs != 2 {
		t.Errorf("site blobs = %d, want 2", report.Site.DistinctBlobs)
	}
}

// A group is a view over other feeds. Counting its members' content as its
// own would double every number on the site.
func TestAGroupIsNotAnInventory(t *testing.T) {
	store := newStore(t)
	writeManifest(t, store, "manifests/npmjs/pkg/-/pkg-1.0.0.tgz", storedManifest{
		SHA256: "aaa", Size: 100, Coordinate: "npm:pkg@1.0.0", Origin: "proxy",
	})

	report := scan(t, store,
		Feed{Name: "npmjs", Format: "npm"},
		Feed{Name: "npm-public", Format: "npm", Group: true})

	for _, u := range report.Feeds {
		if u.Feed == "npm-public" {
			t.Fatalf("the group was given an inventory of its own: %+v", u)
		}
	}
	if report.Site.DistinctBytes != 100 {
		t.Errorf("site bytes = %d, want 100 counted once", report.Site.DistinctBytes)
	}
}

// A published manifest whose database row is gone — restored from an older
// backup, say — is still hosted content, and reporting it as cache would
// suggest deleting it is free.
func TestPublishedManifestsAreCountedAsHosted(t *testing.T) {
	store := newStore(t)
	writeManifest(t, store, "manifests/releases/com/example/lib/1.0.0/lib-1.0.0.jar",
		storedManifest{
			SHA256: "aaa", Size: 500, Coordinate: "maven:com.example:lib@1.0.0", Origin: "publish",
		})

	u := find(t, scan(t, store, Feed{Name: "releases", Format: "maven"}), "releases")
	if u.HostedArtifacts != 1 || u.HostedBytes != 500 || u.HostedPackages != 1 {
		t.Errorf("hosted = %+v, want one artifact of 500 bytes", u)
	}
	if u.CachedArtifacts != 0 {
		t.Errorf("published content was counted as cache: %+v", u)
	}
}

// Manifests written before the coordinate was recorded still count as
// artifacts and bytes. Undercounting packages is a gap that closes as the
// cache turns over; undercounting bytes would be a wrong disk bill.
func TestManifestsWithoutACoordinateStillCount(t *testing.T) {
	store := newStore(t)
	writeManifest(t, store, "manifests/npmjs/old/-/old-1.0.0.tgz", storedManifest{
		SHA256: "aaa", Size: 100, Origin: "proxy",
	})

	u := find(t, scan(t, store, Feed{Name: "npmjs", Format: "npm"}), "npmjs")
	if u.CachedArtifacts != 1 || u.CachedBytes != 100 {
		t.Errorf("usage = %+v, want the artifact and its bytes", u)
	}
	if u.CachedPackages != 0 {
		t.Errorf("packages = %d, want none: the coordinate is not known", u.CachedPackages)
	}
}

// A feed that is no longer configured must not keep appearing. Its manifests
// are still in the store until the collector runs, and reporting them would
// resurrect a feed nobody can reach.
func TestContentOfRemovedFeedsIsNotReported(t *testing.T) {
	store := newStore(t)
	writeManifest(t, store, "manifests/deleted-feed/pkg/-/pkg-1.0.0.tgz", storedManifest{
		SHA256: "aaa", Size: 100, Coordinate: "npm:pkg@1.0.0", Origin: "proxy",
	})

	report := scan(t, store, Feed{Name: "npmjs", Format: "npm"})
	for _, u := range report.Feeds {
		if u.Feed == "deleted-feed" {
			t.Fatalf("a feed that is not configured was reported: %+v", u)
		}
	}
	u := find(t, report, "npmjs")
	if u.Artifacts() != 0 {
		t.Errorf("another feed's content was attributed to npmjs: %+v", u)
	}
}

// One unreadable manifest is a gap in a number. Abandoning the pass would
// mean a single corrupt object costs every feed its inventory.
func TestAnUnreadableManifestDoesNotAbandonTheScan(t *testing.T) {
	store := newStore(t)
	writeManifest(t, store, "manifests/npmjs/good/-/good-1.0.0.tgz", storedManifest{
		SHA256: "aaa", Size: 100, Coordinate: "npm:good@1.0.0", Origin: "proxy",
	})
	if err := store.Put(t.Context(), "manifests/npmjs/broken/-/broken-1.0.0.tgz",
		bytes.NewReader([]byte("not json")), api.PutOpts{}); err != nil {
		t.Fatal(err)
	}

	u := find(t, scan(t, store, Feed{Name: "npmjs", Format: "npm"}), "npmjs")
	if u.CachedArtifacts != 1 || u.CachedBytes != 100 {
		t.Errorf("usage = %+v, want the readable manifest counted", u)
	}
}
