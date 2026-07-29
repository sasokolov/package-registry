package terraform

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fondaco-dev/fondaco/core/api"
)

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
	for _, m := range f.manifests {
		if m.Path == req.Path {
			if m.SHA256 == req.SHA256 {
				return api.PublishResult{SHA256: req.SHA256}, nil
			}
			return api.PublishResult{}, api.ErrImmutable
		}
	}
	f.manifests = append(f.manifests, api.HostedManifest{
		Path: req.Path, Coord: req.Coord, SHA256: req.SHA256, Size: req.Size,
		Metadata: req.Metadata, PublishedAt: time.Unix(0, 0),
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

func gzipBytes(t *testing.T, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func upload(t *testing.T, core *fakeCore, path string, body []byte) error {
	t.Helper()
	r := httptest.NewRequest("PUT", "/"+path, bytes.NewReader(body))
	return Module{}.HandlePublish(t.Context(), api.Feed{Name: "hosted", Format: "terraform"}, r, core)
}

func TestTerraformPublish(t *testing.T) {
	core := newFakeCore()
	archive := gzipBytes(t, "module content")
	path := "v1/modules/ns/mod/generic/1.0.0/archive.tar.gz"

	if err := upload(t, core, path, archive); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if len(core.manifests) != 1 {
		t.Fatalf("manifests = %d", len(core.manifests))
	}
	m := core.manifests[0]
	if m.Coord.Name != "ns/mod/generic" || m.Coord.Version != "1.0.0" {
		t.Errorf("coord = %+v", m.Coord)
	}
	if len(core.blobs) != 1 {
		t.Error("archive not staged as a blob")
	}

	// Immutability and idempotency.
	if err := upload(t, core, path, archive); err != nil {
		t.Errorf("identical re-upload failed: %v", err)
	}
	if err := upload(t, core, path, gzipBytes(t, "different")); !errors.Is(err, api.ErrImmutable) {
		t.Errorf("re-upload with different content = %v, want ErrImmutable", err)
	}
}

func TestTerraformPublishRejectsBadInput(t *testing.T) {
	core := newFakeCore()
	archive := gzipBytes(t, "x")
	for _, path := range []string{
		"v1/modules/ns/mod/generic/1.0.0/module.zip",
		"v1/modules/ns/mod/1.0.0/archive.tar.gz",
		"v1/modules/ns/../generic/1.0.0/archive.tar.gz",
	} {
		if err := upload(t, core, path, archive); !errors.Is(err, api.ErrBadRequest) {
			t.Errorf("upload(%q) = %v, want ErrBadRequest", path, err)
		}
	}
	// Not a gzip archive.
	if err := upload(t, core, "v1/modules/ns/mod/generic/1.0.0/archive.tar.gz",
		[]byte("plain text")); !errors.Is(err, api.ErrBadRequest) {
		t.Errorf("non-gzip upload = %v, want ErrBadRequest", err)
	}
}

func TestTerraformReindexIsDeterministic(t *testing.T) {
	core := newFakeCore()
	for _, v := range []string{"1.0.0", "2.0.0"} {
		if err := upload(t, core, "v1/modules/ns/mod/generic/"+v+"/archive.tar.gz",
			gzipBytes(t, "module "+v)); err != nil {
			t.Fatal(err)
		}
	}
	feed := api.Feed{Name: "hosted", Format: "terraform"}
	if err := (Module{}).Reindex(t.Context(), feed, core); err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	doc := core.indexes["v1/modules/ns/mod/generic/versions"]
	if len(doc) == 0 {
		t.Fatal("no versions document generated")
	}
	for _, want := range []string{`"ns/mod/generic"`, `"1.0.0"`, `"2.0.0"`} {
		if !bytes.Contains(doc, []byte(want)) {
			t.Errorf("versions document lacks %s:\n%s", want, doc)
		}
	}
	if err := (Module{}).Reindex(t.Context(), feed, core); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(doc, core.indexes["v1/modules/ns/mod/generic/versions"]) {
		t.Error("Reindex is not deterministic")
	}
}

// A published archive has to land on the path a GET for it resolves to.
// They are not the same string — Terraform downloads through an indirection
// — and when they drifted apart, every hosted module was a 404 while
// publishing reported success.
func TestPublishedArchiveIsWhereARequestLooksForIt(t *testing.T) {
	const requestPath = "/v1/modules/testns/mymod/generic/1.0.0/archive.tar.gz"

	core := newFakeCore()
	r := httptest.NewRequest("PUT", requestPath, bytes.NewReader(gzipBytes(t, "module")))
	if err := (Module{}).HandlePublish(t.Context(),
		api.Feed{Name: "hosted", Format: "terraform"}, r, core); err != nil {
		t.Fatalf("HandlePublish: %v", err)
	}

	intent, err := Module{}.Parse(httptest.NewRequest("GET", requestPath, nil))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	for _, m := range core.manifests {
		if m.Path == intent.RemotePath {
			return
		}
	}
	published := make([]string, 0, len(core.manifests))
	for _, m := range core.manifests {
		published = append(published, m.Path)
	}
	t.Fatalf("a GET resolves to %q, but publishing stored %v", intent.RemotePath, published)
}
