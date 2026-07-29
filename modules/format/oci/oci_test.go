package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fondaco-dev/fondaco/core/api"
)

func parse(t *testing.T, path string) (api.Intent, error) {
	t.Helper()
	return Module{}.Parse(httptest.NewRequest("GET", path, nil))
}

// An image reference is the URL. Getting this mapping wrong does not produce
// an error anywhere — it produces a registry that no client can address.
func TestRouteToFeed(t *testing.T) {
	tests := []struct {
		path     string
		feed     string
		feedPath string
		ok       bool
	}{
		{"/v2/oci/hub/library/alpine/manifests/3.20", "hub", "/v2/library/alpine/manifests/3.20", true},
		{"/v2/oci/hub/app/blobs/sha256:" + strings.Repeat("a", 64), "hub",
			"/v2/app/blobs/sha256:" + strings.Repeat("a", 64), true},
		{"/v2/oci/hub/app/tags/list", "hub", "/v2/app/tags/list", true},
		// Not this format's business.
		{"/v2/npm/hub/thing", "", "", false},
		{"/v2/", "", "", false},
		{"/v2/oci/hub", "", "", false},
		{"/maven/central/a/b.jar", "", "", false},
	}
	for _, tc := range tests {
		feed, feedPath, ok := Module{}.RouteToFeed(tc.path)
		if ok != tc.ok || feed != tc.feed || feedPath != tc.feedPath {
			t.Errorf("RouteToFeed(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.path, feed, feedPath, ok, tc.feed, tc.feedPath, tc.ok)
		}
	}
}

func TestParse(t *testing.T) {
	digest := "sha256:" + strings.Repeat("ab", 32)
	tests := []struct {
		path    string
		kind    api.IntentKind
		name    string
		version string
	}{
		{"/v2/library/alpine/manifests/3.20", api.IntentMetadata, "library/alpine", "3.20"},
		{"/v2/library/alpine/manifests/" + digest, api.IntentArtifact, "library/alpine", digest},
		{"/v2/library/alpine/blobs/" + digest, api.IntentArtifact, "library/alpine", ""},
		{"/v2/library/alpine/tags/list", api.IntentSearch, "library/alpine", ""},
		{"/v2/_catalog", api.IntentSearch, "", ""},
		// A repository may legitimately have a segment called "manifests";
		// the LAST separator is the real one.
		{"/v2/team/manifests/blobs/manifests/1.0", api.IntentMetadata, "team/manifests/blobs", "1.0"},
	}
	for _, tc := range tests {
		intent, err := parse(t, tc.path)
		if err != nil {
			t.Errorf("Parse(%q): %v", tc.path, err)
			continue
		}
		if intent.Kind != tc.kind {
			t.Errorf("Parse(%q).Kind = %q, want %q", tc.path, intent.Kind, tc.kind)
		}
		if intent.Coord.Name != tc.name || intent.Coord.Version != tc.version {
			t.Errorf("Parse(%q) coord = %q@%q, want %q@%q",
				tc.path, intent.Coord.Name, intent.Coord.Version, tc.name, tc.version)
		}
	}
}

// The digest is in the URL, so an artifact is verified against it without
// asking anything else — this protocol never needs checksum discovery.
func TestADigestInThePathIsTheExpectedChecksum(t *testing.T) {
	hex := strings.Repeat("cd", 32)
	for _, path := range []string{
		"/v2/app/blobs/sha256:" + hex,
		"/v2/app/manifests/sha256:" + hex,
	} {
		intent, err := parse(t, path)
		if err != nil {
			t.Fatalf("Parse(%q): %v", path, err)
		}
		if intent.Checksum.Algo != "sha256" || intent.Checksum.Hex != hex {
			t.Errorf("Parse(%q) checksum = %v, want sha256:%s", path, intent.Checksum, hex)
		}
	}
}

// A tag moves, a digest does not: they must not be cached the same way.
func TestATagIsMutableAndADigestIsNot(t *testing.T) {
	tag, err := parse(t, "/v2/app/manifests/latest")
	if err != nil {
		t.Fatal(err)
	}
	if tag.Kind != api.IntentMetadata || tag.CacheTTL <= 0 {
		t.Errorf("a tag is %s with TTL %v; it has to expire", tag.Kind, tag.CacheTTL)
	}
	if tag.Accept == "" {
		t.Error("a manifest request without an Accept header gets the wrong document")
	}
	digest, err := parse(t, "/v2/app/manifests/sha256:"+strings.Repeat("00", 32))
	if err != nil {
		t.Fatal(err)
	}
	if digest.Kind != api.IntentArtifact {
		t.Errorf("a manifest addressed by digest is %s, want an immutable artifact", digest.Kind)
	}
}

func TestParseRefusesNonsense(t *testing.T) {
	for _, p := range []string{
		"/v2/",
		"/v2/app/blobs/not-a-digest",
		"/v2/app/manifests/",
		"/v2/UPPERCASE/manifests/1.0",
		"/v2/app/../../etc/passwd",
		"/whatever",
	} {
		if _, err := parse(t, p); err == nil {
			t.Errorf("Parse(%q) was accepted", p)
		}
	}
}

// A manifest's identity is the sha256 of its exact bytes. Re-encoding it —
// even into equivalent JSON — would change the digest every client verifies
// it against, and the image would stop being the image.
func TestAManifestIsNeverRewritten(t *testing.T) {
	body := []byte("{\n  \"schemaVersion\": 2,\n  \"mediaType\": \"" + mediaTypeOCIManifest + "\"\n}")
	out, err := Module{}.RewriteMetadata(api.Feed{Name: "hub"}, body)
	if err != nil {
		t.Fatalf("RewriteMetadata: %v", err)
	}
	if !bytes.Equal(out, body) {
		t.Fatalf("the bytes changed:\n got %q\nwant %q", out, body)
	}
}

func TestAnUpstreamErrorPageIsNotCachedAsAnImage(t *testing.T) {
	_, err := Module{}.RewriteMetadata(api.Feed{Name: "hub"}, []byte("<html>404</html>"))
	if err == nil {
		t.Fatal("an HTML error page was accepted as a manifest")
	}
}

// The digest header is how a client pulling by tag learns what it got.
func TestTheDigestIsAnnouncedOnManifests(t *testing.T) {
	hex := strings.Repeat("ef", 32)
	intent, err := parse(t, "/v2/app/manifests/latest")
	if err != nil {
		t.Fatal(err)
	}
	headers := Module{}.ResponseHeaders(api.Feed{Name: "hub"}, intent, hex)
	if headers["Docker-Content-Digest"] != "sha256:"+hex {
		t.Errorf("Docker-Content-Digest = %q", headers["Docker-Content-Digest"])
	}
	if headers["Docker-Distribution-API-Version"] != "registry/2.0" {
		t.Error("clients check the API version header before anything else")
	}
	// Nothing is claimed when nothing is known.
	empty := Module{}.ResponseHeaders(api.Feed{Name: "hub"}, intent, "")
	if got := empty["Docker-Content-Digest"]; got != "" {
		t.Errorf("a digest was announced without one being known: %q", got)
	}
}

// A group that answered from its first member would hide every tag the other
// members have, and a pull of a tag the group demonstrably serves would fail.
func TestGroupTagListsAreUnioned(t *testing.T) {
	intent, err := parse(t, "/v2/app/tags/list")
	if err != nil {
		t.Fatal(err)
	}
	out, err := Module{}.Merge(api.Feed{Name: "public"}, intent, []api.GroupPart{
		{Feed: "hosted", Body: []byte(`{"name":"app","tags":["2.0","1.0"]}`)},
		{Feed: "proxy", Body: []byte(`{"name":"app","tags":["1.0","0.9"]}`)},
		{Feed: "empty", Body: []byte(`{"name":"app","tags":null}`)},
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	var doc struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Name != "app" {
		t.Errorf("name = %q", doc.Name)
	}
	if want := []string{"0.9", "1.0", "2.0"}; !equalStrings(doc.Tags, want) {
		t.Errorf("tags = %v, want %v", doc.Tags, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Pushing

// A push is a conversation: every answer says where the next request goes.
// Without the Location and Range headers a client cannot continue at all.
func TestAnUploadIsCarriedOnFromWhatIsStaged(t *testing.T) {
	core := newFakeCore()
	feed := hostedFeed()
	layer := []byte("a layer, in two halves")

	// Start.
	rec := do(t, core, feed, "POST", "/v2/app/blobs/uploads/", nil, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST uploads/ = %d, want 202", rec.Code)
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, "/v2/oci/internal/app/blobs/uploads/") {
		t.Fatalf("Location = %q; a client resolves the next request against it", location)
	}
	if rec.Header().Get("Range") != "0-0" {
		t.Errorf("Range = %q, want 0-0 for an empty session", rec.Header().Get("Range"))
	}
	session := feedRelative(t, location)

	// Two chunks.
	rec = do(t, core, feed, "PATCH", session, layer[:10], nil)
	if rec.Code != http.StatusAccepted || rec.Header().Get("Range") != "0-9" {
		t.Fatalf("first chunk: %d %q", rec.Code, rec.Header().Get("Range"))
	}
	rec = do(t, core, feed, "PATCH", session, layer[10:], nil)
	if want := fmt.Sprintf("0-%d", len(layer)-1); rec.Header().Get("Range") != want {
		t.Fatalf("second chunk Range = %q, want %q", rec.Header().Get("Range"), want)
	}

	// Commit.
	digest := "sha256:" + sha256Hex(layer)
	rec = do(t, core, feed, "PUT", session+"?digest="+digest, nil, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("commit = %d, want 201", rec.Code)
	}
	if rec.Header().Get("Docker-Content-Digest") != digest {
		t.Errorf("Docker-Content-Digest = %q", rec.Header().Get("Docker-Content-Digest"))
	}
	if got := core.blobs[blobStoreKey(sha256Hex(layer))]; !bytes.Equal(got, layer) {
		t.Fatalf("the assembled blob is %q, want %q", got, layer)
	}
	if !core.published(blobPath("app", digest)) {
		t.Error("the blob was staged but never became a coordinate")
	}
	// Staging is cleaned up; an unfinished upload must not linger as
	// storage nobody will ever look at again.
	for key := range core.blobs {
		if strings.HasPrefix(key, api.StagingPrefix) {
			t.Errorf("staged chunk %s survived the commit", key)
		}
	}
}

// A layer that arrived corrupted must not exist at all: it is what every
// image containing it would be verified against.
func TestAnUploadThatDoesNotMatchItsDigestIsRefused(t *testing.T) {
	core := newFakeCore()
	feed := hostedFeed()

	rec := do(t, core, feed, "POST", "/v2/app/blobs/uploads/", nil, nil)
	session := feedRelative(t, rec.Header().Get("Location"))
	do(t, core, feed, "PATCH", session, []byte("the real bytes"), nil)

	wrong := "sha256:" + strings.Repeat("11", 32)
	err := Module{}.HandlePublishHTTP(t.Context(), feed, httptest.NewRecorder(),
		request("PUT", session+"?digest="+wrong, nil, nil), core)
	if !errors.Is(err, api.ErrChecksumMismatch) {
		t.Fatalf("err = %v, want ErrChecksumMismatch", err)
	}
	if _, ok := core.blobs[blobStoreKey(strings.Repeat("11", 32))]; ok {
		t.Error("a blob that failed verification is visible in the store")
	}
	if core.published(blobPath("app", wrong)) {
		t.Error("a blob that failed verification became a coordinate")
	}
}

// A manifest referencing bytes the feed does not have is not an image; it is
// a pull that fails halfway through, for everyone, forever.
func TestAManifestWithoutItsBlobsIsRefused(t *testing.T) {
	core := newFakeCore()
	feed := hostedFeed()
	manifest := imageManifest(strings.Repeat("aa", 32), strings.Repeat("bb", 32))

	err := Module{}.HandlePublishHTTP(t.Context(), feed, httptest.NewRecorder(),
		request("PUT", "/v2/app/manifests/1.0", manifest, map[string]string{
			"Content-Type": mediaTypeOCIManifest,
		}), core)
	if !errors.Is(err, api.ErrBadRequest) {
		t.Fatalf("err = %v, want ErrBadRequest", err)
	}
	if core.published(manifestPath("app", "1.0")) {
		t.Error("an image nobody can pull was published anyway")
	}
}

// The release is the digest and the tag is a pointer at it. Both have to
// exist, and only one of them may ever move (invariant 4).
func TestAnImageIsPublishedAtItsDigestAndAtItsTag(t *testing.T) {
	core := newFakeCore()
	feed := hostedFeed()
	config, layer := stageBlob(t, core, feed, "app", []byte(`{"architecture":"amd64"}`)),
		stageBlob(t, core, feed, "app", []byte("layer bytes"))
	manifest := imageManifest(config, layer)
	digest := sha256Hex(manifest)

	rec := do(t, core, feed, "PUT", "/v2/app/manifests/1.0", manifest, map[string]string{
		"Content-Type": mediaTypeOCIManifest,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT manifest = %d, want 201", rec.Code)
	}
	if got := rec.Header().Get("Docker-Content-Digest"); got != "sha256:"+digest {
		t.Errorf("Docker-Content-Digest = %q, want sha256:%s", got, digest)
	}

	byDigest := core.row(manifestPath("app", "sha256:"+digest))
	if byDigest == nil {
		t.Fatal("the manifest was not published at its digest")
	}
	if byDigest.mutable {
		t.Error("a manifest at its digest is a release and must not be mutable")
	}
	if byDigest.metadata[api.MetaContentType] != mediaTypeOCIManifest {
		t.Errorf("media type recorded as %q; a client dispatches on it",
			byDigest.metadata[api.MetaContentType])
	}
	tag := core.row(manifestPath("app", "1.0"))
	if tag == nil || tag.sha256 != digest {
		t.Fatal("the tag does not point at the manifest")
	}
	if !tag.mutable {
		t.Error("a tag that cannot move is not a tag")
	}
}

// Retagging is the protocol working as designed; what must not change is
// what the digest resolves to.
func TestATagMovesAndTheDigestDoesNot(t *testing.T) {
	core := newFakeCore()
	feed := hostedFeed()
	first := imageManifest(
		stageBlob(t, core, feed, "app", []byte(`{"architecture":"amd64"}`)),
		stageBlob(t, core, feed, "app", []byte("v1 layer")))
	second := imageManifest(
		stageBlob(t, core, feed, "app", []byte(`{"architecture":"arm64"}`)),
		stageBlob(t, core, feed, "app", []byte("v2 layer")))

	for _, body := range [][]byte{first, second} {
		rec := do(t, core, feed, "PUT", "/v2/app/manifests/latest", body,
			map[string]string{"Content-Type": mediaTypeOCIManifest})
		if rec.Code != http.StatusCreated {
			t.Fatalf("PUT = %d", rec.Code)
		}
	}

	if got := core.row(manifestPath("app", "latest")).sha256; got != sha256Hex(second) {
		t.Errorf("the tag did not move: %s", got)
	}
	if row := core.row(manifestPath("app", "sha256:"+sha256Hex(first))); row == nil || row.sha256 != sha256Hex(first) {
		t.Error("the first image stopped being reachable at its own digest")
	}
}

// Deleting is what a registry is asked for when an image must go away, and
// answering "no" is only useful if it says what to do instead.
func TestDeletingAPublishedImageIsRefusedWithSomewhereToGo(t *testing.T) {
	core := newFakeCore()
	err := Module{}.HandlePublishHTTP(t.Context(), hostedFeed(), httptest.NewRecorder(),
		request("DELETE", "/v2/app/manifests/1.0", nil, nil), core)
	if !errors.Is(err, api.ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	if !strings.Contains(err.Error(), "quarantine") {
		t.Errorf("the refusal does not say what to do instead: %v", err)
	}
}

// An unfinished upload is staging, not content: cancelling it is allowed and
// must actually free what it staged.
func TestCancellingAnUploadDiscardsIt(t *testing.T) {
	core := newFakeCore()
	feed := hostedFeed()
	rec := do(t, core, feed, "POST", "/v2/app/blobs/uploads/", nil, nil)
	session := feedRelative(t, rec.Header().Get("Location"))
	do(t, core, feed, "PATCH", session, []byte("half an upload"), nil)

	rec = do(t, core, feed, "DELETE", session, nil, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("cancel = %d, want 204", rec.Code)
	}
	for key := range core.blobs {
		if strings.HasPrefix(key, api.StagingPrefix) {
			t.Errorf("staged chunk %s survived the cancellation", key)
		}
	}
}

// ---------------------------------------------------------------------------
// Listing

func TestListingsComeFromWhatIsPublished(t *testing.T) {
	core := newFakeCore()
	feed := hostedFeed()
	publishImage(t, core, feed, "app", "1.0")
	publishImage(t, core, feed, "app", "2.0")
	publishImage(t, core, feed, "team/other", "1.0")

	intent, err := parse(t, "/v2/app/tags/list")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := Module{}.Search(t.Context(), feed, intent, core)
	if err != nil {
		t.Fatal(err)
	}
	var tags struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(resp.Body, &tags); err != nil {
		t.Fatal(err)
	}
	if !equalStrings(tags.Tags, []string{"1.0", "2.0"}) {
		t.Errorf("tags = %v", tags.Tags)
	}

	catalogIntent, err := parse(t, "/v2/_catalog")
	if err != nil {
		t.Fatal(err)
	}
	resp, err = Module{}.Search(t.Context(), feed, catalogIntent, core)
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Repositories []string `json:"repositories"`
	}
	if err := json.Unmarshal(resp.Body, &catalog); err != nil {
		t.Fatal(err)
	}
	if !equalStrings(catalog.Repositories, []string{"app", "team/other"}) {
		t.Errorf("repositories = %v", catalog.Repositories)
	}
}

// An empty tag list would tell a client the image was deleted; the protocol
// has a status for "no such repository" and it is not 200.
func TestAnUnknownRepositoryIsNotAnEmptyList(t *testing.T) {
	core := newFakeCore()
	intent, err := parse(t, "/v2/nothing/here/tags/list")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := Module{}.Search(t.Context(), hostedFeed(), intent, core)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.Status)
	}
}

func TestPaginationWalksTheWholeList(t *testing.T) {
	core := newFakeCore()
	feed := hostedFeed()
	for _, tag := range []string{"a", "b", "c"} {
		publishImage(t, core, feed, "app", tag)
	}

	var seen []string
	query := "n=2"
	for range 5 {
		intent, err := Module{}.Parse(httptest.NewRequest("GET", "/v2/app/tags/list?"+query, nil))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := Module{}.Search(t.Context(), feed, intent, core)
		if err != nil {
			t.Fatal(err)
		}
		var page struct {
			Tags []string `json:"tags"`
		}
		if err := json.Unmarshal(resp.Body, &page); err != nil {
			t.Fatal(err)
		}
		seen = append(seen, page.Tags...)
		link := resp.Header["Link"]
		if link == "" {
			break
		}
		query = strings.TrimSuffix(strings.TrimPrefix(link, "<?"), `>; rel="next"`)
	}
	if !equalStrings(seen, []string{"a", "b", "c"}) {
		t.Errorf("walking the pages saw %v", seen)
	}
}

// ---------------------------------------------------------------------------
// Helpers

func hostedFeed() api.Feed {
	return api.Feed{
		Name: "internal", Format: formatName, Hosted: true,
		Anonymous: true, ExternalURL: "http://registry.example",
	}
}

// feedRelative turns a Location a client was given back into the path the
// module parses, which is exactly what the core does when the request comes
// back in (api.FeedRouter).
func feedRelative(t *testing.T, location string) string {
	t.Helper()
	feed, path, ok := Module{}.RouteToFeed(location)
	if !ok || feed != "internal" {
		t.Fatalf("Location %q does not route back to the feed", location)
	}
	return path
}

func request(method, path string, body []byte, headers map[string]string) *http.Request {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	r := httptest.NewRequest(method, path, reader)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func do(t *testing.T, core *fakeCore, feed api.Feed, method, path string,
	body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := (Module{}).HandlePublishHTTP(t.Context(), feed, rec, request(method, path, body, headers), core); err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return rec
}

// imageManifest is a real OCI manifest naming a config and one layer.
func imageManifest(configDigest, layerDigest string) []byte {
	return []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,`+
		`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:%s","size":1},`+
		`"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":"sha256:%s","size":1}]}`,
		mediaTypeOCIManifest, configDigest, layerDigest))
}

// stageBlob puts a blob where a push would have left it and returns its hex
// digest.
func stageBlob(t *testing.T, core *fakeCore, feed api.Feed, repo string, body []byte) string {
	t.Helper()
	digest := sha256Hex(body)
	if err := core.Blobs().Put(t.Context(), blobStoreKey(digest), bytes.NewReader(body),
		api.PutOpts{SHA256: digest}); err != nil {
		t.Fatal(err)
	}
	if err := publishBlob(t.Context(), feed, core, repo, digest, int64(len(body))); err != nil {
		t.Fatal(err)
	}
	return digest
}

// publishImage pushes a complete image, the way a client would.
func publishImage(t *testing.T, core *fakeCore, feed api.Feed, repo, tag string) {
	t.Helper()
	config := stageBlob(t, core, feed, repo, []byte(`{"repo":"`+repo+`","tag":"`+tag+`"}`))
	layer := stageBlob(t, core, feed, repo, []byte("layer of "+repo+":"+tag))
	do(t, core, feed, "PUT", "/v2/"+repo+"/manifests/"+tag, imageManifest(config, layer),
		map[string]string{"Content-Type": mediaTypeOCIManifest})
}

// fakeCore is an in-memory api.CoreServices.
type fakeCore struct {
	mu    sync.Mutex
	blobs map[string][]byte
	rows  []hostedRow
}

type hostedRow struct {
	path     string
	coord    api.PackageCoordinate
	sha256   string
	size     int64
	mutable  bool
	metadata map[string]string
}

func newFakeCore() *fakeCore { return &fakeCore{blobs: map[string][]byte{}} }

func (f *fakeCore) row(path string) *hostedRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.rows {
		if f.rows[i].path == path {
			return &f.rows[i]
		}
	}
	return nil
}

func (f *fakeCore) published(path string) bool { return f.row(path) != nil }

func (f *fakeCore) Blobs() api.BlobStore { return (*fakeBlobs)(f) }

func (f *fakeCore) Publish(_ context.Context, req api.PublishRequest) (api.PublishResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.rows {
		if f.rows[i].path != req.Path {
			continue
		}
		if f.rows[i].sha256 == req.SHA256 {
			return api.PublishResult{SHA256: req.SHA256}, nil
		}
		if !f.rows[i].mutable || !req.Mutable {
			return api.PublishResult{}, api.ErrImmutable
		}
		f.rows[i].sha256 = req.SHA256
		return api.PublishResult{SHA256: req.SHA256}, nil
	}
	f.rows = append(f.rows, hostedRow{
		path: req.Path, coord: req.Coord, sha256: req.SHA256, size: req.Size,
		mutable: req.Mutable, metadata: req.Metadata,
	})
	return api.PublishResult{Created: true, SHA256: req.SHA256}, nil
}

func (f *fakeCore) Manifests(_ context.Context, _ api.Feed, prefix string) ([]api.HostedManifest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []api.HostedManifest
	for _, r := range f.rows {
		if !strings.HasPrefix(r.path, prefix) {
			continue
		}
		out = append(out, api.HostedManifest{
			Path: r.path, Coord: r.coord, SHA256: r.sha256, Size: r.size,
			Metadata: r.metadata, Site: "test", Publisher: "token:test",
			PublishedAt: time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (f *fakeCore) PutIndex(context.Context, api.Feed, string, []byte) error {
	return errors.New("this format generates no indexes")
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

// Put verifies the digest exactly as a real store does, because "nothing
// becomes visible on a mismatch" is the property under test.
func (b *fakeBlobs) Put(_ context.Context, key string, r io.Reader, opts api.PutOpts) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if opts.SHA256 != "" && !strings.EqualFold(opts.SHA256, sha256Hex(data)) {
		return fmt.Errorf("put %s: %w", key, api.ErrChecksumMismatch)
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

func (b *fakeBlobs) Delete(_ context.Context, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.blobs, key)
	return nil
}

func (b *fakeBlobs) List(_ context.Context, prefix string) (api.Iter[api.BlobInfo], error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var keys []string
	for key := range b.blobs {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	items := make([]api.BlobInfo, 0, len(keys))
	for _, key := range keys {
		items = append(items, api.BlobInfo{Key: key, Size: int64(len(b.blobs[key]))})
	}
	return &sliceIter{items: items}, nil
}

type sliceIter struct {
	items []api.BlobInfo
	i     int
}

func (s *sliceIter) Next(context.Context) (api.BlobInfo, bool) {
	if s.i >= len(s.items) {
		return api.BlobInfo{}, false
	}
	item := s.items[s.i]
	s.i++
	return item, true
}

func (s *sliceIter) Err() error { return nil }
