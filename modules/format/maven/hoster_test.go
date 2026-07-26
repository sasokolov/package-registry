package maven

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	published time.Time
}

func newFakeCore() *fakeCore {
	return &fakeCore{
		blobs:     map[string][]byte{},
		indexes:   map[string][]byte{},
		published: time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC),
	}
}

func (f *fakeCore) Blobs() api.BlobStore { return (*fakeBlobs)(f) }

func (f *fakeCore) Publish(_ context.Context, req api.PublishRequest) (api.PublishResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.manifests {
		if m.Path == req.Path {
			if m.SHA256 == req.SHA256 {
				return api.PublishResult{Created: false, SHA256: req.SHA256}, nil
			}
			return api.PublishResult{}, api.ErrImmutable
		}
	}
	f.manifests = append(f.manifests, api.HostedManifest{
		Path: req.Path, Coord: req.Coord, SHA256: req.SHA256, Size: req.Size,
		Checksums: req.Checksums, Metadata: req.Metadata,
		Site: "test", Publisher: "token:test", PublishedAt: f.published,
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

// fakeBlobs implements api.BlobStore over fakeCore's map.
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

func put(t *testing.T, core *fakeCore, path, body string) error {
	t.Helper()
	r := httptest.NewRequest("PUT", "/"+path, strings.NewReader(body))
	return Module{}.HandlePublish(t.Context(), api.Feed{Name: "hosted", Format: "maven"}, r, core)
}

const pomBody = `<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
  <groupId>com.example</groupId>
  <artifactId>lib</artifactId>
  <version>1.0.0</version>
  <licenses>
    <license><name>Apache-2.0</name></license>
  </licenses>
</project>`

func TestHandlePublish(t *testing.T) {
	core := newFakeCore()

	if err := put(t, core, "com/example/lib/1.0.0/lib-1.0.0.jar", "jar bytes"); err != nil {
		t.Fatalf("publish jar: %v", err)
	}
	if err := put(t, core, "com/example/lib/1.0.0/lib-1.0.0.pom", pomBody); err != nil {
		t.Fatalf("publish pom: %v", err)
	}

	sum := sha256.Sum256([]byte("jar bytes"))
	if _, ok := core.blobs["blobs/sha256/"+hex.EncodeToString(sum[:])]; !ok {
		t.Error("jar not staged as a content-addressed blob")
	}
	if len(core.manifests) != 2 {
		t.Fatalf("manifests = %d, want 2", len(core.manifests))
	}
	for _, m := range core.manifests {
		if m.Checksums["sha1"] == "" || m.Checksums["md5"] == "" {
			t.Errorf("%s: sidecar digests missing: %+v", m.Path, m.Checksums)
		}
		if m.Metadata[api.MetaEcosystem] != "Maven" {
			t.Errorf("%s: ecosystem = %q", m.Path, m.Metadata[api.MetaEcosystem])
		}
	}
	// The pom's license reached canonical metadata for the policies.
	var pomMeta map[string]string
	for _, m := range core.manifests {
		if strings.HasSuffix(m.Path, ".pom") {
			pomMeta = m.Metadata
		}
	}
	if pomMeta[api.MetaLicense] != "Apache-2.0" {
		t.Errorf("pom license = %q", pomMeta[api.MetaLicense])
	}
}

func TestHandlePublishImmutability(t *testing.T) {
	core := newFakeCore()
	path := "com/example/lib/1.0.0/lib-1.0.0.jar"
	if err := put(t, core, path, "original"); err != nil {
		t.Fatal(err)
	}
	// Identical content is an idempotent retry.
	if err := put(t, core, path, "original"); err != nil {
		t.Errorf("identical republish failed: %v", err)
	}
	// Different content on a release coordinate is a conflict.
	if err := put(t, core, path, "tampered"); !errors.Is(err, api.ErrImmutable) {
		t.Errorf("republish with different content = %v, want ErrImmutable", err)
	}
}

func TestHandlePublishChecksumSidecar(t *testing.T) {
	core := newFakeCore()
	path := "com/example/lib/1.0.0/lib-1.0.0.jar"
	if err := put(t, core, path, "jar bytes"); err != nil {
		t.Fatal(err)
	}
	stored := core.manifests[0].Checksums["sha1"]

	// A matching sidecar upload is accepted and not stored as a coordinate.
	if err := put(t, core, path+".sha1", stored+"  lib-1.0.0.jar\n"); err != nil {
		t.Errorf("matching sidecar rejected: %v", err)
	}
	if len(core.manifests) != 1 {
		t.Errorf("sidecar became a coordinate: %d manifests", len(core.manifests))
	}
	// A mismatching one means the upload was corrupted.
	if err := put(t, core, path+".sha1", strings.Repeat("0", 40)); !errors.Is(err, api.ErrChecksumMismatch) {
		t.Errorf("mismatching sidecar = %v, want ErrChecksumMismatch", err)
	}
}

func TestHandlePublishIgnoresClientMetadata(t *testing.T) {
	core := newFakeCore()
	if err := put(t, core, "com/example/lib/maven-metadata.xml", "<metadata/>"); err != nil {
		t.Fatalf("metadata upload rejected: %v", err)
	}
	if len(core.manifests) != 0 {
		t.Error("client-uploaded maven-metadata.xml was stored; Reindex owns it")
	}
}

func TestReindexIsDeterministic(t *testing.T) {
	core := newFakeCore()
	for _, v := range []string{"1.0.0", "1.10.0", "1.2.0", "2.0.0"} {
		if err := put(t, core, "com/example/lib/"+v+"/lib-"+v+".jar", "jar "+v); err != nil {
			t.Fatal(err)
		}
	}
	if err := put(t, core, "com/example/other/1.0.0/other-1.0.0.jar", "other"); err != nil {
		t.Fatal(err)
	}

	feed := api.Feed{Name: "hosted", Format: "maven"}
	if err := (Module{}).Reindex(t.Context(), feed, core); err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	first := core.indexes["com/example/lib/maven-metadata.xml"]
	if len(first) == 0 {
		t.Fatal("no maven-metadata.xml generated")
	}
	if _, ok := core.indexes["com/example/other/maven-metadata.xml"]; !ok {
		t.Error("second artifact has no metadata")
	}

	// Byte-identical on a second run: this property is what lets geo
	// replication rebuild indexes locally instead of replicating them.
	if err := (Module{}).Reindex(t.Context(), feed, core); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, core.indexes["com/example/lib/maven-metadata.xml"]) {
		t.Error("Reindex is not deterministic")
	}

	doc := string(first)
	if !strings.Contains(doc, "<latest>2.0.0</latest>") || !strings.Contains(doc, "<release>2.0.0</release>") {
		t.Errorf("latest/release wrong:\n%s", doc)
	}
	// Maven version ordering is numeric per segment: 1.10.0 > 1.2.0.
	if strings.Index(doc, "<version>1.2.0</version>") > strings.Index(doc, "<version>1.10.0</version>") {
		t.Errorf("version ordering is lexicographic:\n%s", doc)
	}

	golden, err := os.ReadFile(filepath.Join("testdata", "reindex-golden.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(golden) {
		t.Errorf("generated index differs from golden:\n%s", first)
	}
}

func TestCompareVersions(t *testing.T) {
	ordered := []string{"1.0", "1.0.1", "1.2.0", "1.10.0", "2.0.0"}
	for i := 1; i < len(ordered); i++ {
		if compareVersions(ordered[i-1], ordered[i]) >= 0 {
			t.Errorf("%s should sort before %s", ordered[i-1], ordered[i])
		}
	}
	if compareVersions("1.0.0", "1.0.0") != 0 {
		t.Error("equal versions must compare equal")
	}
	if compareVersions("1.0-rc1", "1.0") >= 0 {
		t.Error("qualifier should sort before the plain version")
	}
}

func TestMetadataSource(t *testing.T) {
	m := Module{}
	intent, ok := m.MetadataIntent(api.Feed{Name: "central"},
		api.PackageCoordinate{Format: "maven", Name: "com.example:lib", Version: "1.0.0"})
	if !ok {
		t.Fatal("no metadata intent for a versioned coordinate")
	}
	if intent.RemotePath != "com/example/lib/1.0.0/lib-1.0.0.pom" {
		t.Errorf("pom path = %q", intent.RemotePath)
	}
	if _, ok := m.MetadataIntent(api.Feed{}, api.PackageCoordinate{Name: "com.example:lib"}); ok {
		t.Error("version-less coordinate got a metadata intent")
	}

	meta, err := m.ExtractMetadata([]byte(pomBody))
	if err != nil {
		t.Fatal(err)
	}
	if meta[api.MetaLicense] != "Apache-2.0" || meta[api.MetaEcosystem] != "Maven" {
		t.Errorf("extracted metadata = %+v", meta)
	}
}
