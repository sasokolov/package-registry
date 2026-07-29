package nuget

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fondaco-dev/fondaco/core/api"
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

// hostedFeed is a feed that can accept pushes and knows its own address.
func hostedFeed() api.Feed {
	return api.Feed{
		Name: "internal", Format: "nuget", Hosted: true,
		Anonymous: true, ExternalURL: "http://registry.example",
	}
}

// nupkg builds a real .nupkg: a zip with a .nuspec at its root, which is all
// the registry reads.
func nupkg(t *testing.T, id, version, extra string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	spec := `<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://schemas.microsoft.com/packaging/2013/05/nuspec.xsd">
  <metadata>
    <id>` + id + `</id>
    <version>` + version + `</version>
    <authors>conformance</authors>
    <description>a test package</description>
    <license type="expression">MIT</license>
` + extra + `  </metadata>
</package>
`
	writeEntry(t, zw, id+".nuspec", spec)
	// A content file, so the archive looks like a real package and the
	// root-only nuspec rule has something to ignore.
	writeEntry(t, zw, "lib/net8.0/"+id+".dll", "not really an assembly")
	writeEntry(t, zw, "lib/net8.0/nested.nuspec", "<package/>")
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func writeEntry(t *testing.T, zw *zip.Writer, name, body string) {
	t.Helper()
	header := &zip.FileHeader{Name: name, Method: zip.Deflate}
	// A fixed timestamp: the published date is derived from the archive, so
	// a wall clock here would make the generated documents unstable.
	header.Modified = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	w, err := zw.CreateHeader(header)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if _, err := io.WriteString(w, body); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// push sends a package the way `dotnet nuget push` does: multipart.
func push(t *testing.T, core *fakeCore, feed api.Feed, body []byte) error {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("package", "package.nupkg")
	if err != nil {
		t.Fatalf("build multipart: %v", err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}

	r := httptest.NewRequest("PUT", publishPath, &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	return Module{}.HandlePublish(t.Context(), feed, r, core)
}

func reindex(t *testing.T, core *fakeCore, feed api.Feed) {
	t.Helper()
	if err := (Module{}).Reindex(t.Context(), feed, core); err != nil {
		t.Fatalf("Reindex: %v", err)
	}
}

// A pushed package must be downloadable at exactly the path a NuGet client
// asks for, which is the lowercased flat-container path.
func TestPushStoresThePackageWhereClientsAskForIt(t *testing.T) {
	core := newFakeCore()
	feed := hostedFeed()
	if err := push(t, core, feed, nupkg(t, "Conformance.Lib", "1.2.3", "")); err != nil {
		t.Fatalf("push: %v", err)
	}

	want := map[string]bool{
		"v3-flatcontainer/conformance.lib/1.2.3/conformance.lib.1.2.3.nupkg": false,
		"v3-flatcontainer/conformance.lib/1.2.3/conformance.lib.nuspec":      false,
		"-hosted/conformance.lib/1.2.3.json":                                 false,
	}
	for _, m := range core.manifests {
		if _, ok := want[m.Path]; ok {
			want[m.Path] = true
		}
	}
	for path, found := range want {
		if !found {
			t.Errorf("nothing was published at %s; got %v", path, paths(core))
		}
	}
}

// The nuspec is read from the archive root. A content file that happens to
// end in .nuspec must not be mistaken for the manifest.
func TestPushReadsTheManifestFromTheArchiveRoot(t *testing.T) {
	core := newFakeCore()
	if err := push(t, core, hostedFeed(), nupkg(t, "pkg", "1.0.0", "")); err != nil {
		t.Fatalf("push: %v", err)
	}
	for _, m := range core.manifests {
		if !strings.HasSuffix(m.Path, "pkg.nuspec") {
			continue
		}
		body := core.blobs["blobs/sha256/"+m.SHA256]
		if !strings.Contains(string(body), "<id>pkg</id>") {
			t.Fatalf("the stored manifest is not the package's own: %q", body)
		}
		return
	}
	t.Fatal("no nuspec was published")
}

// The license reaches policies through the canonical key, or a license
// policy on a hosted feed would have nothing to judge.
func TestPushRecordsTheLicenseForPolicies(t *testing.T) {
	core := newFakeCore()
	if err := push(t, core, hostedFeed(), nupkg(t, "pkg", "1.0.0", "")); err != nil {
		t.Fatalf("push: %v", err)
	}
	for _, m := range core.manifests {
		if m.Metadata[api.MetaEcosystem] != "NuGet" {
			t.Errorf("%s: ecosystem = %q", m.Path, m.Metadata[api.MetaEcosystem])
		}
		if m.Metadata[api.MetaLicense] != "MIT" {
			t.Errorf("%s: license = %q, want MIT", m.Path, m.Metadata[api.MetaLicense])
		}
	}
}

func TestReindexListsEveryVersionInOrder(t *testing.T) {
	core := newFakeCore()
	feed := hostedFeed()
	for _, v := range []string{"1.10.0", "1.9.0", "2.0.0-rc.1", "2.0.0"} {
		if err := push(t, core, feed, nupkg(t, "pkg", v, "")); err != nil {
			t.Fatalf("push %s: %v", v, err)
		}
	}
	reindex(t, core, feed)

	var index struct {
		Versions []string `json:"versions"`
	}
	body := core.indexes["v3-flatcontainer/pkg/index.json"]
	if body == nil {
		t.Fatalf("no flat index was generated; got %v", indexPaths(core))
	}
	if err := json.Unmarshal(body, &index); err != nil {
		t.Fatalf("parse flat index: %v", err)
	}
	got := strings.Join(index.Versions, ",")
	// Ordered as versions, not as text: 1.9.0 before 1.10.0, and the
	// pre-release before its release.
	if want := "1.9.0,1.10.0,2.0.0-rc.1,2.0.0"; got != want {
		t.Errorf("versions = %q, want %q", got, want)
	}
}

// The registration document is what `dotnet restore` resolves from, so its
// URLs have to point at this registry and its dependencies have to survive.
func TestReindexBuildsAResolvableRegistration(t *testing.T) {
	core := newFakeCore()
	feed := hostedFeed()
	deps := `    <dependencies>
      <group targetFramework=".NETStandard2.0">
        <dependency id="Newtonsoft.Json" version="13.0.1" />
      </group>
    </dependencies>
`
	if err := push(t, core, feed, nupkg(t, "Pkg", "1.0.0", deps)); err != nil {
		t.Fatalf("push: %v", err)
	}
	reindex(t, core, feed)

	body := core.indexes["v3/registration5-gz-semver2/pkg/index.json"]
	if body == nil {
		t.Fatalf("no registration was generated; got %v", indexPaths(core))
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse registration: %v", err)
	}

	page := doc["items"].([]any)[0].(map[string]any)
	leaf := page["items"].([]any)[0].(map[string]any)
	catalog := leaf["catalogEntry"].(map[string]any)

	const wantContent = "http://registry.example/nuget/internal/v3/flat2/pkg/1.0.0/pkg.1.0.0.nupkg"
	if leaf["packageContent"] != wantContent {
		t.Errorf("packageContent = %v, want %v", leaf["packageContent"], wantContent)
	}
	if catalog["id"] != "Pkg" {
		t.Errorf("the catalog entry lost the package's own casing: %v", catalog["id"])
	}
	if catalog["licenseExpression"] != "MIT" {
		t.Errorf("licenseExpression = %v", catalog["licenseExpression"])
	}
	groups, ok := catalog["dependencyGroups"].([]any)
	if !ok || len(groups) != 1 {
		t.Fatalf("dependencyGroups = %v, want one group", catalog["dependencyGroups"])
	}
	group := groups[0].(map[string]any)
	if group["targetFramework"] != ".NETStandard2.0" {
		t.Errorf("targetFramework = %v", group["targetFramework"])
	}
	dep := group["dependencies"].([]any)[0].(map[string]any)
	if dep["id"] != "Newtonsoft.Json" || dep["range"] != "13.0.1" {
		t.Errorf("dependency = %v", dep)
	}
}

// Rebuilding must be a pure function of what was published: a geo peer that
// received only manifests has to produce the same documents this site did.
func TestReindexIsDeterministic(t *testing.T) {
	core := newFakeCore()
	feed := hostedFeed()
	for _, v := range []string{"1.0.0", "2.0.0"} {
		if err := push(t, core, feed, nupkg(t, "pkg", v, "")); err != nil {
			t.Fatalf("push %s: %v", v, err)
		}
	}

	reindex(t, core, feed)
	first := map[string]string{}
	for path, body := range core.indexes {
		first[path] = string(body)
	}
	for i := 0; i < 3; i++ {
		reindex(t, core, feed)
		for path, body := range core.indexes {
			if first[path] != string(body) {
				t.Fatalf("rebuild %d changed %s:\n%s\n---\n%s", i, path, first[path], body)
			}
		}
	}
}

// Republishing the same version with different bytes is refused: a release
// is immutable, and NuGet clients cache by version.
func TestRepublishingAVersionIsRefused(t *testing.T) {
	core := newFakeCore()
	feed := hostedFeed()
	if err := push(t, core, feed, nupkg(t, "pkg", "1.0.0", "")); err != nil {
		t.Fatalf("first push: %v", err)
	}
	err := push(t, core, feed, nupkg(t, "pkg", "1.0.0", "    <projectUrl>https://example.com</projectUrl>\n"))
	if !errors.Is(err, api.ErrImmutable) {
		t.Fatalf("republish error = %v, want ErrImmutable", err)
	}
}

// Pushing the identical bytes again is a client retry, not a conflict.
func TestPushingTheSameBytesTwiceIsFine(t *testing.T) {
	core := newFakeCore()
	feed := hostedFeed()
	body := nupkg(t, "pkg", "1.0.0", "")
	if err := push(t, core, feed, body); err != nil {
		t.Fatalf("first push: %v", err)
	}
	if err := push(t, core, feed, body); err != nil {
		t.Fatalf("identical re-push: %v", err)
	}
}

func TestPushRejectsWhatIsNotAPackage(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{name: "not a zip", body: []byte("hello")},
		{name: "empty", body: []byte{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			core := newFakeCore()
			err := push(t, core, hostedFeed(), tc.body)
			if !errors.Is(err, api.ErrBadRequest) {
				t.Fatalf("error = %v, want ErrBadRequest", err)
			}
			if len(core.manifests) != 0 {
				t.Errorf("a rejected push published %v", paths(core))
			}
		})
	}
}

// A zip without a manifest is not a package, whatever its extension says.
func TestPushRejectsAZipWithoutAManifest(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	writeEntry(t, zw, "lib/net8.0/thing.dll", "x")
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	err := push(t, newFakeCore(), hostedFeed(), buf.Bytes())
	if !errors.Is(err, api.ErrBadRequest) {
		t.Fatalf("error = %v, want ErrBadRequest", err)
	}
}

// Without an external URL there is no honest way to write the absolute URLs
// registration documents must carry, so the push is refused rather than
// producing a package nobody can resolve.
func TestPushNeedsToKnowTheSiteAddress(t *testing.T) {
	feed := hostedFeed()
	feed.ExternalURL = ""
	err := push(t, newFakeCore(), feed, nupkg(t, "pkg", "1.0.0", ""))
	if !errors.Is(err, api.ErrBadRequest) {
		t.Fatalf("error = %v, want ErrBadRequest", err)
	}
	if !strings.Contains(err.Error(), "external_url") {
		t.Errorf("the refusal does not say what is missing: %v", err)
	}
}

// Only a feed that accepts writes advertises where to send them.
func TestServiceIndexAdvertisesPublishOnlyWhenHosted(t *testing.T) {
	intent := api.Intent{Kind: api.IntentSynthetic}

	hosted, err := Module{}.Synthesize(hostedFeed(), intent)
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if !strings.Contains(string(hosted.Body), "PackagePublish/2.0.0") {
		t.Error("a hosted feed does not advertise its push endpoint")
	}
	if !strings.Contains(string(hosted.Body), "http://registry.example/nuget/internal/api/v2/package") {
		t.Errorf("the push endpoint is wrong:\n%s", hosted.Body)
	}

	proxy := hostedFeed()
	proxy.Hosted = false
	proxied, err := Module{}.Synthesize(proxy, intent)
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if strings.Contains(string(proxied.Body), "PackagePublish") {
		t.Error("a proxy feed advertises a push endpoint it cannot serve")
	}
}

func paths(core *fakeCore) []string {
	out := make([]string, 0, len(core.manifests))
	for _, m := range core.manifests {
		out = append(out, m.Path)
	}
	return out
}

func indexPaths(core *fakeCore) []string {
	out := make([]string, 0, len(core.indexes))
	for path := range core.indexes {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Search

func search(t *testing.T, core *fakeCore, feed api.Feed, query string) map[string]any {
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

func ids(doc map[string]any) []string {
	data, _ := doc["data"].([]any)
	out := make([]string, 0, len(data))
	for _, entry := range data {
		id, _ := entry.(map[string]any)["id"].(string)
		out = append(out, id)
	}
	return out
}

func TestSearchFindsWhatTheFeedHosts(t *testing.T) {
	core := newFakeCore()
	feed := hostedFeed()
	for _, id := range []string{"Acme.Logging", "Acme.Http", "Other.Thing"} {
		if err := push(t, core, feed, nupkg(t, id, "1.0.0", "")); err != nil {
			t.Fatalf("push %s: %v", id, err)
		}
	}

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{name: "no term lists everything", query: "", want: []string{"Acme.Http", "Acme.Logging", "Other.Thing"}},
		{name: "a prefix narrows it", query: "q=acme", want: []string{"Acme.Http", "Acme.Logging"}},
		{name: "an exact id comes first", query: "q=acme.logging", want: []string{"Acme.Logging"}},
		{name: "the search is case-insensitive", query: "q=OTHER", want: []string{"Other.Thing"}},
		{name: "nothing matching is an empty answer, not an error", query: "q=nosuchthing", want: []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := search(t, core, feed, tc.query)
			got := strings.Join(ids(doc), ",")
			if want := strings.Join(tc.want, ","); got != want {
				t.Errorf("ids = %q, want %q", got, want)
			}
			if hits, _ := doc["totalHits"].(float64); int(hits) != len(tc.want) {
				t.Errorf("totalHits = %v, want %d", doc["totalHits"], len(tc.want))
			}
		})
	}
}

// A search answer must agree with what a restore would get, so it reports
// the newest version and lists the rest.
func TestSearchReportsTheNewestVersionAndListsTheRest(t *testing.T) {
	core := newFakeCore()
	feed := hostedFeed()
	for _, v := range []string{"1.9.0", "1.10.0", "2.0.0-rc.1"} {
		if err := push(t, core, feed, nupkg(t, "pkg", v, "")); err != nil {
			t.Fatalf("push %s: %v", v, err)
		}
	}

	doc := search(t, core, feed, "q=pkg")
	entry := doc["data"].([]any)[0].(map[string]any)
	if entry["version"] != "1.10.0" {
		t.Errorf("version = %v, want 1.10.0 (the newest release, compared as a version)", entry["version"])
	}
	if got := len(entry["versions"].([]any)); got != 3 {
		t.Errorf("versions listed = %d, want 3", got)
	}

	// A pre-release is only the answer when it was asked for.
	doc = search(t, core, feed, "q=pkg&prerelease=true")
	entry = doc["data"].([]any)[0].(map[string]any)
	if entry["version"] != "2.0.0-rc.1" {
		t.Errorf("with prerelease=true version = %v, want the pre-release", entry["version"])
	}
}

// A package whose only version is a pre-release must not appear in a search
// that did not ask for pre-releases: a restore would not take it either.
func TestSearchHidesPreReleaseOnlyPackagesByDefault(t *testing.T) {
	core := newFakeCore()
	feed := hostedFeed()
	if err := push(t, core, feed, nupkg(t, "preview.only", "0.1.0-alpha", "")); err != nil {
		t.Fatalf("push: %v", err)
	}
	if got := ids(search(t, core, feed, "")); len(got) != 0 {
		t.Errorf("ids = %v, want none", got)
	}
	if got := ids(search(t, core, feed, "prerelease=true")); len(got) != 1 {
		t.Errorf("ids = %v, want the pre-release package", got)
	}
}

func TestSearchPaginates(t *testing.T) {
	core := newFakeCore()
	feed := hostedFeed()
	for _, id := range []string{"a.one", "a.two", "a.three"} {
		if err := push(t, core, feed, nupkg(t, id, "1.0.0", "")); err != nil {
			t.Fatalf("push %s: %v", id, err)
		}
	}
	doc := search(t, core, feed, "take=2")
	if got := len(ids(doc)); got != 2 {
		t.Errorf("take=2 returned %d", got)
	}
	if hits, _ := doc["totalHits"].(float64); int(hits) != 3 {
		t.Errorf("totalHits = %v, want the full count regardless of the page", doc["totalHits"])
	}
	doc = search(t, core, feed, "skip=2")
	if got := len(ids(doc)); got != 1 {
		t.Errorf("skip=2 returned %d", got)
	}
	// Skipping past the end is an empty page, not a panic.
	if got := len(ids(search(t, core, feed, "skip=99"))); got != 0 {
		t.Errorf("skip past the end returned %d", got)
	}
}

// The query has to reach the module, or a proxy asks its upstream nothing
// and caches that one answer for every search anybody runs.
func TestSearchIntentCarriesTheQuery(t *testing.T) {
	r := httptest.NewRequest("GET", "/v3/query?q=acme&take=5", nil)
	intent, err := Module{}.Parse(r)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if intent.Kind != api.IntentSearch {
		t.Errorf("kind = %v, want search", intent.Kind)
	}
	if intent.RemoteQuery != "q=acme&take=5" {
		t.Errorf("RemoteQuery = %q", intent.RemoteQuery)
	}
}
