package npm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sasokolov/package-registry/core/api"
)

// fakeCore is an in-memory api.CoreServices for module tests.
type fakeCore struct {
	mu        sync.Mutex
	blobs     map[string][]byte
	manifests []api.HostedManifest
	indexes   map[string][]byte
}

func newFakeCore() *fakeCore {
	return &fakeCore{blobs: map[string][]byte{}, indexes: map[string][]byte{}}
}

func (f *fakeCore) Blobs() api.BlobStore { return (*fakeBlobs)(f) }

func (f *fakeCore) Publish(_ context.Context, req api.PublishRequest) (api.PublishResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, m := range f.manifests {
		if m.Path != req.Path {
			continue
		}
		if m.SHA256 == req.SHA256 {
			return api.PublishResult{Created: false, SHA256: req.SHA256}, nil
		}
		if !req.Mutable {
			return api.PublishResult{}, api.ErrImmutable
		}
		f.manifests[i].SHA256 = req.SHA256
		return api.PublishResult{Created: false, SHA256: req.SHA256}, nil
	}
	f.manifests = append(f.manifests, api.HostedManifest{
		Path: req.Path, Coord: req.Coord, SHA256: req.SHA256, Size: req.Size,
		Checksums: req.Checksums, Metadata: req.Metadata,
		Site: "test", Publisher: "token:test",
		PublishedAt: time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC),
	})
	return api.PublishResult{Created: true, SHA256: req.SHA256}, nil
}

func (f *fakeCore) Manifests(_ context.Context, _ api.Feed, prefix string) ([]api.HostedManifest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []api.HostedManifest
	for _, m := range f.manifests {
		if strings.HasPrefix(m.Path, prefix) {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (f *fakeCore) PutIndex(_ context.Context, _ api.Feed, path string, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.indexes[path] = append([]byte(nil), body...)
	return nil
}

func (f *fakeCore) Site() string { return "test" }

type fakeBlobs fakeCore

func (b *fakeBlobs) Get(_ context.Context, key string) (io.ReadCloser, api.BlobInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.blobs[key]
	if !ok {
		return nil, api.BlobInfo{}, api.NotFoundf("blob %s", key)
	}
	return io.NopCloser(bytes.NewReader(data)), api.BlobInfo{Key: key, Size: int64(len(data))}, nil
}

func (b *fakeBlobs) Put(_ context.Context, key string, r io.Reader, _ api.PutOpts) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.blobs[key] = data
	return nil
}

func (b *fakeBlobs) Stat(_ context.Context, key string) (api.BlobInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.blobs[key]
	if !ok {
		return api.BlobInfo{}, api.NotFoundf("blob %s", key)
	}
	return api.BlobInfo{Key: key, Size: int64(len(data))}, nil
}

func (b *fakeBlobs) Delete(context.Context, string) error { return nil }
func (b *fakeBlobs) List(context.Context, string) (api.Iter[api.BlobInfo], error) {
	return nil, errors.New("not implemented")
}

// publishPackage publishes one version the way `npm publish` does: the whole
// package document with the tarball inline as a base64 attachment.
func publishPackage(t *testing.T, core *fakeCore, feed api.Feed, name, version, description string) error {
	t.Helper()
	tarball := name + "-" + version + ".tgz"
	doc := map[string]any{
		"name":      name,
		"dist-tags": map[string]any{"latest": version},
		"versions": map[string]any{
			version: map[string]any{
				"name":        name,
				"version":     version,
				"description": description,
				"license":     "MIT",
				"keywords":    []any{"conformance"},
				"dist":        map[string]any{"tarball": "http://example/" + name + "/-/" + tarball},
			},
		},
		"_attachments": map[string]any{
			tarball: map[string]any{
				"content_type": "application/octet-stream",
				"data":         base64.StdEncoding.EncodeToString([]byte("tarball of " + name + " " + version)),
			},
		},
	}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal publish document: %v", err)
	}
	r := httptest.NewRequest("PUT", "/"+name, bytes.NewReader(body))
	return Module{}.HandlePublish(t.Context(), feed, r, core)
}

// ---------------------------------------------------------------------------
// Search

func npmSearch(t *testing.T, core *fakeCore, feed api.Feed, query string) map[string]any {
	t.Helper()
	resp, err := Module{}.Search(t.Context(), feed,
		api.Intent{Kind: api.IntentSearch, RemoteQuery: query}, core)
	if err != nil {
		t.Fatalf("Search(%q): %v", query, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatalf("parse search results: %v\n%s", err, resp.Body)
	}
	return doc
}

func searchNames(doc map[string]any) []string {
	objects, _ := doc["objects"].([]any)
	out := make([]string, 0, len(objects))
	for _, o := range objects {
		pkg, _ := o.(map[string]any)["package"].(map[string]any)
		name, _ := pkg["name"].(string)
		out = append(out, name)
	}
	return out
}

func TestSearchFindsWhatTheFeedHosts(t *testing.T) {
	core := newFakeCore()
	feed := api.Feed{Name: "internal", Format: "npm", Hosted: true}
	for _, name := range []string{"acme-logger", "acme-http", "unrelated"} {
		if err := publishPackage(t, core, feed, name, "1.0.0", "a "+name+" package"); err != nil {
			t.Fatalf("publish %s: %v", name, err)
		}
	}

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{name: "no term lists everything", query: "", want: []string{"acme-http", "acme-logger", "unrelated"}},
		{name: "a prefix narrows it", query: "text=acme", want: []string{"acme-http", "acme-logger"}},
		{name: "an exact name comes first", query: "text=acme-logger", want: []string{"acme-logger"}},
		{name: "the description is searched too", query: "text=unrelated", want: []string{"unrelated"}},
		{name: "nothing matching is an empty answer", query: "text=nosuchthing", want: []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := npmSearch(t, core, feed, tc.query)
			got := strings.Join(searchNames(doc), ",")
			if want := strings.Join(tc.want, ","); got != want {
				t.Errorf("names = %q, want %q", got, want)
			}
			if total, _ := doc["total"].(float64); int(total) != len(tc.want) {
				t.Errorf("total = %v, want %d", doc["total"], len(tc.want))
			}
		})
	}
}

// A search answer must agree with what an install would get: the newest
// version, compared as a version rather than as text.
func TestSearchReportsTheNewestVersion(t *testing.T) {
	core := newFakeCore()
	feed := api.Feed{Name: "internal", Format: "npm", Hosted: true}
	for _, v := range []string{"1.9.0", "1.10.0"} {
		if err := publishPackage(t, core, feed, "pkg", v, "a package"); err != nil {
			t.Fatalf("publish %s: %v", v, err)
		}
	}
	doc := npmSearch(t, core, feed, "text=pkg")
	object := doc["objects"].([]any)[0].(map[string]any)
	pkg := object["package"].(map[string]any)
	if pkg["version"] != "1.10.0" {
		t.Errorf("version = %v, want 1.10.0", pkg["version"])
	}
}

func TestSearchPaginates(t *testing.T) {
	core := newFakeCore()
	feed := api.Feed{Name: "internal", Format: "npm", Hosted: true}
	for _, name := range []string{"a-one", "a-two", "a-three"} {
		if err := publishPackage(t, core, feed, name, "1.0.0", "x"); err != nil {
			t.Fatalf("publish %s: %v", name, err)
		}
	}
	doc := npmSearch(t, core, feed, "size=2")
	if got := len(searchNames(doc)); got != 2 {
		t.Errorf("size=2 returned %d", got)
	}
	if total, _ := doc["total"].(float64); int(total) != 3 {
		t.Errorf("total = %v, want the full count regardless of the page", doc["total"])
	}
	if got := len(searchNames(npmSearch(t, core, feed, "from=99"))); got != 0 {
		t.Errorf("paging past the end returned %d", got)
	}
}

// The query has to reach the module, or a proxy asks its upstream nothing.
func TestSearchIntentCarriesTheQuery(t *testing.T) {
	r := httptest.NewRequest("GET", "/-/v1/search?text=acme&size=5", nil)
	intent, err := Module{}.Parse(r)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if intent.Kind != api.IntentSearch {
		t.Errorf("kind = %v, want search", intent.Kind)
	}
	if intent.RemoteQuery != "text=acme&size=5" {
		t.Errorf("RemoteQuery = %q", intent.RemoteQuery)
	}
}

// A scoped package is spelled two ways, and npm uses both in one publish: the
// attachment keeps the scope, the tarball path drops it. Storing under the
// upload name leaves the index advertising a URL that answers 404 — the
// package publishes "successfully" and cannot be installed.
func TestScopedPackagesArePublishedWhereTheyAreFetched(t *testing.T) {
	tests := []struct {
		pkg      string
		file     string
		version  string
		wantPath string
	}{
		{
			pkg: "@sindresorhus/slugify", file: "@sindresorhus/slugify-3.0.0.tgz",
			version:  "3.0.0",
			wantPath: "@sindresorhus/slugify/-/slugify-3.0.0.tgz",
		},
		{
			// The same package, uploaded by a client that spells the
			// attachment the short way.
			pkg: "@sindresorhus/slugify", file: "slugify-3.0.0.tgz",
			version:  "3.0.0",
			wantPath: "@sindresorhus/slugify/-/slugify-3.0.0.tgz",
		},
		{
			pkg: "left-pad", file: "left-pad-1.3.0.tgz",
			version:  "1.3.0",
			wantPath: "left-pad/-/left-pad-1.3.0.tgz",
		},
	}
	for _, tc := range tests {
		version := versionFromTarball(tc.pkg, tc.file)
		if version != tc.version {
			t.Errorf("versionFromTarball(%q, %q) = %q, want %q",
				tc.pkg, tc.file, version, tc.version)
			continue
		}
		if got := tarballPath(tc.pkg, tarballFile(tc.pkg, version)); got != tc.wantPath {
			t.Errorf("stored path for %s = %q, want %q", tc.pkg, got, tc.wantPath)
		}
	}
}

// A filename that belongs to another package must not be accepted as a
// version: it used to come back whole, and then travelled into a coordinate.
func TestAnAttachmentFromAnotherPackageIsRefused(t *testing.T) {
	for _, file := range []string{"something-else-1.0.0.tgz", "1.0.0.tgz", "slugify-.tgz"} {
		if version := versionFromTarball("@sindresorhus/slugify", file); version != "" {
			t.Errorf("versionFromTarball(%q) = %q, want it refused", file, version)
		}
	}
}
