package composer

import (
	"archive/zip"
	"bytes"
	"context"
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

func hostedFeed() api.Feed {
	return api.Feed{
		Name: "internal", Format: "composer", Hosted: true,
		Anonymous: true, ExternalURL: "http://registry.example",
	}
}

// dist builds an ordinary Composer archive: everything under one directory,
// with composer.json inside it, exactly as `composer archive` produces.
// The archive is always acme/lib's; what varies between tests is the
// manifest inside it.
func dist(t *testing.T, manifest string) []byte {
	t.Helper()
	return distNamed(t, "acme/lib", manifest)
}

// distNamed builds an archive for a given package name, wrapped in one
// directory the way `composer archive` produces.
func distNamed(t *testing.T, name, manifest string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	root := strings.ReplaceAll(name, "/", "-") + "/"
	writeEntry(t, zw, root+"composer.json", manifest)
	writeEntry(t, zw, root+"src/thing.php", "<?php // a package")
	// A vendored dependency's manifest, deeper in: it must not be mistaken
	// for this package's own.
	writeEntry(t, zw, root+"vendor/other/pkg/composer.json", `{"name":"other/pkg"}`)
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func writeEntry(t *testing.T, zw *zip.Writer, name, body string) {
	t.Helper()
	header := &zip.FileHeader{Name: name, Method: zip.Deflate}
	header.Modified = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	w, err := zw.CreateHeader(header)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if _, err := io.WriteString(w, body); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func upload(t *testing.T, core *fakeCore, feed api.Feed, vendor, name, version string, body []byte) error {
	t.Helper()
	target := "/" + hostedDistPrefix + vendor + "/" + name + "/" + version + ".zip"
	r := httptest.NewRequest("PUT", target, bytes.NewReader(body))
	return Module{}.HandlePublish(t.Context(), feed, r, core)
}

func reindex(t *testing.T, core *fakeCore, feed api.Feed) {
	t.Helper()
	if err := (Module{}).Reindex(t.Context(), feed, core); err != nil {
		t.Fatalf("Reindex: %v", err)
	}
}

const libManifest = `{
  "name": "acme/lib",
  "type": "library",
  "description": "a hosted package",
  "license": "MIT",
  "require": {"php": ">=8.1"},
  "autoload": {"files": ["src/thing.php"]}
}`

// You PUT the archive where it will be served from, so the download path is
// the upload path and there is nothing to look up.
func TestUploadStoresTheArchiveWhereItIsServed(t *testing.T) {
	core := newFakeCore()
	if err := upload(t, core, hostedFeed(), "acme", "lib", "1.0.0", dist(t, libManifest)); err != nil {
		t.Fatalf("upload: %v", err)
	}

	want := "packages/acme/lib/1.0.0.zip"
	for _, m := range core.manifests {
		if m.Path == want {
			// And the same path parses back to the same coordinate.
			intent, err := Module{}.Parse(httptest.NewRequest("GET", "/"+want, nil))
			if err != nil {
				t.Fatalf("the stored path does not parse back: %v", err)
			}
			if intent.Coord.Name != "acme/lib" || intent.Coord.Version != "1.0.0" {
				t.Errorf("coordinate = %v", intent.Coord)
			}
			return
		}
	}
	t.Fatalf("nothing was published at %s; got %v", want, paths(core))
}

// The manifest is this package's, not a vendored dependency's.
func TestUploadReadsThePackagesOwnManifest(t *testing.T) {
	core := newFakeCore()
	if err := upload(t, core, hostedFeed(), "acme", "lib", "1.0.0", dist(t, libManifest)); err != nil {
		t.Fatalf("upload: %v", err)
	}
	reindex(t, core, hostedFeed())

	doc := p2Doc(t, core, "acme/lib")
	entry := doc["packages"].(map[string]any)["acme/lib"].([]any)[0].(map[string]any)
	if entry["name"] != "acme/lib" {
		t.Errorf("name = %v", entry["name"])
	}
	if entry["description"] != "a hosted package" {
		t.Errorf("description = %v", entry["description"])
	}
}

// A path and an archive that disagree about the package name is a mistake
// worth refusing, not resolving by guessing.
func TestUploadRefusesAMismatchedName(t *testing.T) {
	core := newFakeCore()
	err := upload(t, core, hostedFeed(), "other", "name", "1.0.0", dist(t, libManifest))
	if !errors.Is(err, api.ErrBadRequest) {
		t.Fatalf("error = %v, want ErrBadRequest", err)
	}
	if len(core.manifests) != 0 {
		t.Errorf("a refused upload published %v", paths(core))
	}
}

func TestUploadRefusesWhatIsNotAPackage(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{name: "not a zip", body: []byte("hello")},
		{name: "empty", body: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := upload(t, newFakeCore(), hostedFeed(), "acme", "lib", "1.0.0", tc.body)
			if !errors.Is(err, api.ErrBadRequest) {
				t.Fatalf("error = %v, want ErrBadRequest", err)
			}
		})
	}
}

func TestUploadRefusesAZipWithoutAManifest(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	writeEntry(t, zw, "acme-lib/src/thing.php", "<?php")
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	err := upload(t, newFakeCore(), hostedFeed(), "acme", "lib", "1.0.0", buf.Bytes())
	if !errors.Is(err, api.ErrBadRequest) {
		t.Fatalf("error = %v, want ErrBadRequest", err)
	}
}

// The p2 document is what Composer resolves from, so its dist URL must point
// here and its require map must survive.
func TestReindexBuildsAResolvableP2Document(t *testing.T) {
	core := newFakeCore()
	feed := hostedFeed()
	if err := upload(t, core, feed, "acme", "lib", "1.0.0", dist(t, libManifest)); err != nil {
		t.Fatalf("upload: %v", err)
	}
	reindex(t, core, feed)

	entry := p2Doc(t, core, "acme/lib")["packages"].(map[string]any)["acme/lib"].([]any)[0].(map[string]any)
	distInfo := entry["dist"].(map[string]any)
	const wantURL = "http://registry.example/composer/internal/packages/acme/lib/1.0.0.zip"
	if distInfo["url"] != wantURL {
		t.Errorf("dist.url = %v, want %v", distInfo["url"], wantURL)
	}
	if distInfo["type"] != "zip" {
		t.Errorf("dist.type = %v", distInfo["type"])
	}
	if distInfo["shasum"] == "" {
		t.Error("dist.shasum is empty; Composer verifies it")
	}
	if entry["version_normalized"] != "1.0.0.0" {
		t.Errorf("version_normalized = %v, want 1.0.0.0", entry["version_normalized"])
	}
	require, ok := entry["require"].(map[string]any)
	if !ok || require["php"] != ">=8.1" {
		t.Errorf("require = %v", entry["require"])
	}
}

func TestReindexOrdersVersionsAsVersions(t *testing.T) {
	core := newFakeCore()
	feed := hostedFeed()
	for _, v := range []string{"1.10.0", "1.9.0", "2.0.0-beta"} {
		if err := upload(t, core, feed, "acme", "lib", v, dist(t, libManifest)); err != nil {
			t.Fatalf("upload %s: %v", v, err)
		}
	}
	reindex(t, core, feed)

	entries := p2Doc(t, core, "acme/lib")["packages"].(map[string]any)["acme/lib"].([]any)
	got := make([]string, 0, len(entries))
	for _, e := range entries {
		got = append(got, e.(map[string]any)["version"].(string))
	}
	if want := "1.9.0,1.10.0,2.0.0-beta"; strings.Join(got, ",") != want {
		t.Errorf("versions = %v, want %s", got, want)
	}
}

// Composer asks for the root manifest first; it has to say where to look
// packages up, even when there are none yet.
func TestReindexBuildsTheRootManifest(t *testing.T) {
	core := newFakeCore()
	feed := hostedFeed()
	reindex(t, core, feed)

	var root map[string]any
	if err := json.Unmarshal(core.indexes["packages.json"], &root); err != nil {
		t.Fatalf("no usable root manifest: %v", err)
	}
	if root["metadata-url"] != "http://registry.example/composer/internal/p2/%package%.json" {
		t.Errorf("metadata-url = %v", root["metadata-url"])
	}

	if err := upload(t, core, feed, "acme", "lib", "1.0.0", dist(t, libManifest)); err != nil {
		t.Fatalf("upload: %v", err)
	}
	reindex(t, core, feed)
	if err := json.Unmarshal(core.indexes["packages.json"], &root); err != nil {
		t.Fatalf("parse root manifest: %v", err)
	}
	available := root["available-packages"].([]any)
	if len(available) != 1 || available[0] != "acme/lib" {
		t.Errorf("available-packages = %v", available)
	}
}

func TestReindexIsDeterministic(t *testing.T) {
	core := newFakeCore()
	feed := hostedFeed()
	for _, v := range []string{"1.0.0", "2.0.0"} {
		if err := upload(t, core, feed, "acme", "lib", v, dist(t, libManifest)); err != nil {
			t.Fatalf("upload %s: %v", v, err)
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

func TestRepublishingAVersionIsRefused(t *testing.T) {
	core := newFakeCore()
	feed := hostedFeed()
	if err := upload(t, core, feed, "acme", "lib", "1.0.0", dist(t, libManifest)); err != nil {
		t.Fatalf("first upload: %v", err)
	}
	changed := strings.Replace(libManifest, "a hosted package", "something else", 1)
	err := upload(t, core, feed, "acme", "lib", "1.0.0", dist(t, changed))
	if !errors.Is(err, api.ErrImmutable) {
		t.Fatalf("error = %v, want ErrImmutable", err)
	}
}

func TestNormalizeVersion(t *testing.T) {
	tests := map[string]string{
		"1.0.0":      "1.0.0.0",
		"v2.1":       "2.1.0.0",
		"1.2.3.4":    "1.2.3.4",
		"2.0.0-BETA": "2.0.0.0-beta",
	}
	for in, want := range tests {
		if got := normalizeVersion(in); got != want {
			t.Errorf("normalizeVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func p2Doc(t *testing.T, core *fakeCore, name string) map[string]any {
	t.Helper()
	body := core.indexes["p2/"+name+".json"]
	if body == nil {
		t.Fatalf("no p2 document for %s; got %v", name, indexPaths(core))
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse p2 document: %v\n%s", err, body)
	}
	return doc
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

func composerSearch(t *testing.T, core *fakeCore, feed api.Feed, query string) map[string]any {
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

func resultNames(doc map[string]any) []string {
	results, _ := doc["results"].([]any)
	out := make([]string, 0, len(results))
	for _, r := range results {
		name, _ := r.(map[string]any)["name"].(string)
		out = append(out, name)
	}
	return out
}

func manifestFor(name, description, packageType string) string {
	return `{
  "name": "` + name + `",
  "type": "` + packageType + `",
  "description": "` + description + `",
  "license": "MIT"
}`
}

func TestSearchFindsWhatTheFeedHosts(t *testing.T) {
	core := newFakeCore()
	feed := hostedFeed()
	packages := []struct{ vendor, name, description, kind string }{
		{"acme", "logger", "structured logging", "library"},
		{"acme", "http", "an http client", "library"},
		{"other", "tool", "a build plugin", "composer-plugin"},
	}
	for _, p := range packages {
		full := p.vendor + "/" + p.name
		archive := distNamed(t, full, manifestFor(full, p.description, p.kind))
		if err := upload(t, core, feed, p.vendor, p.name, "1.0.0", archive); err != nil {
			t.Fatalf("upload %s: %v", full, err)
		}
	}

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{name: "no term lists everything", query: "", want: []string{"acme/http", "acme/logger", "other/tool"}},
		{name: "a prefix narrows it", query: "q=acme", want: []string{"acme/http", "acme/logger"}},
		{name: "an exact name comes first", query: "q=acme/logger", want: []string{"acme/logger"}},
		{name: "the description is searched too", query: "q=logging", want: []string{"acme/logger"}},
		{name: "type filters", query: "type=composer-plugin", want: []string{"other/tool"}},
		{name: "nothing matching is an empty answer", query: "q=nosuchthing", want: []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := composerSearch(t, core, feed, tc.query)
			got := strings.Join(resultNames(doc), ",")
			if want := strings.Join(tc.want, ","); got != want {
				t.Errorf("names = %q, want %q", got, want)
			}
			if total, _ := doc["total"].(float64); int(total) != len(tc.want) {
				t.Errorf("total = %v, want %d", doc["total"], len(tc.want))
			}
		})
	}
}

// Every result must point at a document the client can actually fetch.
func TestSearchResultsPointAtTheirMetadata(t *testing.T) {
	core := newFakeCore()
	feed := hostedFeed()
	if err := upload(t, core, feed, "acme", "lib", "1.0.0", dist(t, libManifest)); err != nil {
		t.Fatalf("upload: %v", err)
	}
	result := composerSearch(t, core, feed, "q=acme")["results"].([]any)[0].(map[string]any)
	const want = "http://registry.example/composer/internal/p2/acme/lib.json"
	if result["url"] != want {
		t.Errorf("url = %v, want %v", result["url"], want)
	}
	// A private registry measures neither, and saying zero beats inventing.
	if result["downloads"].(float64) != 0 || result["favers"].(float64) != 0 {
		t.Errorf("downloads/favers = %v/%v, want zeroes", result["downloads"], result["favers"])
	}
}

func TestSearchPaginates(t *testing.T) {
	core := newFakeCore()
	feed := hostedFeed()
	for _, name := range []string{"one", "two", "three"} {
		full := "acme/" + name
		if err := upload(t, core, feed, "acme", name, "1.0.0",
			distNamed(t, full, manifestFor(full, "x", "library"))); err != nil {
			t.Fatalf("upload %s: %v", full, err)
		}
	}
	doc := composerSearch(t, core, feed, "per_page=2")
	if got := len(resultNames(doc)); got != 2 {
		t.Errorf("per_page=2 returned %d", got)
	}
	if total, _ := doc["total"].(float64); int(total) != 3 {
		t.Errorf("total = %v, want the full count regardless of the page", doc["total"])
	}
	if got := len(resultNames(composerSearch(t, core, feed, "page=99"))); got != 0 {
		t.Errorf("paging past the end returned %d", got)
	}
}

func TestSearchIntentCarriesTheQuery(t *testing.T) {
	r := httptest.NewRequest("GET", "/search.json?q=acme&type=library", nil)
	intent, err := Module{}.Parse(r)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if intent.Kind != api.IntentSearch {
		t.Errorf("kind = %v, want search", intent.Kind)
	}
	if intent.RemoteQuery != "q=acme&type=library" {
		t.Errorf("RemoteQuery = %q", intent.RemoteQuery)
	}
}

// The root manifest has to tell Composer that search exists, or the client
// reports "no repository supports search" and never asks.
func TestHostedRootManifestAdvertisesSearch(t *testing.T) {
	core := newFakeCore()
	reindex(t, core, hostedFeed())

	var root map[string]any
	if err := json.Unmarshal(core.indexes["packages.json"], &root); err != nil {
		t.Fatalf("parse root manifest: %v", err)
	}
	const want = "http://registry.example/composer/internal/search.json?q=%query%&type=%type%"
	if root["search"] != want {
		t.Errorf("search = %v, want %v", root["search"], want)
	}
}
