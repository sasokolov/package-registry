package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sasokolov/package-registry/core/api"
	"github.com/sasokolov/package-registry/modules/storage/fs"
)

// gc is the only command that deletes data, and it had no test at all.
// These pin the rules that keep it from deleting something live.

func gcStore(t *testing.T) (api.BlobStore, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := fs.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return store, dir
}

func putBlob(t *testing.T, store api.BlobStore, content string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	digest := hex.EncodeToString(sum[:])
	if err := store.Put(t.Context(), "blobs/sha256/"+digest,
		strings.NewReader(content), api.PutOpts{SHA256: digest}); err != nil {
		t.Fatal(err)
	}
	return digest
}

func putManifest(t *testing.T, store api.BlobStore, feed, path, digest string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"sha256": digest, "size": 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(t.Context(), "manifests/"+feed+"/"+path,
		bytes.NewReader(body), api.PutOpts{}); err != nil {
		t.Fatal(err)
	}
}

// makeOld backdates a blob past the -min-age floor the tests use. The
// filesystem store keeps object bytes under data/.
func makeOld(t *testing.T, dir, digest string) {
	t.Helper()
	path := filepath.Join(dir, "data", "blobs", "sha256", digest)
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestGCKeepsReferencedAndYoungBlobs(t *testing.T) {
	store, dir := gcStore(t)
	ctx := t.Context()

	referenced := putBlob(t, store, "referenced by a manifest")
	putManifest(t, store, "hosted", "lib/1.0.0/lib.jar", referenced)
	makeOld(t, dir, referenced)

	orphanOld := putBlob(t, store, "nothing points at me and I am old")
	makeOld(t, dir, orphanOld)

	orphanYoung := putBlob(t, store, "nothing points at me but I am new")

	var out bytes.Buffer
	if err := sweep(ctx, store, nil, &out, discard(), true, 24*time.Hour); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if _, err := store.Stat(ctx, "blobs/sha256/"+referenced); err != nil {
		t.Errorf("a referenced blob was collected: %v", err)
	}
	if _, err := store.Stat(ctx, "blobs/sha256/"+orphanYoung); err != nil {
		t.Errorf("a blob younger than -min-age was collected: %v", err)
	}
	if _, err := store.Stat(ctx, "blobs/sha256/"+orphanOld); err == nil {
		t.Error("an old unreferenced blob survived the sweep")
	}
	if !strings.Contains(out.String(), "1 unreferenced") {
		t.Errorf("report does not mention the collected blob:\n%s", out.String())
	}
}

func TestGCDryRunDeletesNothing(t *testing.T) {
	store, dir := gcStore(t)
	ctx := t.Context()

	orphan := putBlob(t, store, "old orphan")
	makeOld(t, dir, orphan)

	var out bytes.Buffer
	if err := sweep(ctx, store, nil, &out, discard(), false, 24*time.Hour); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := store.Stat(ctx, "blobs/sha256/"+orphan); err != nil {
		t.Error("dry run deleted a blob")
	}
	if !strings.Contains(out.String(), "dry-run") {
		t.Errorf("dry run is not labelled as such:\n%s", out.String())
	}
}

// An unreadable manifest could reference anything, so the sweep must abort
// rather than delete on an incomplete picture.
func TestGCRefusesToSweepOnAnUnreadableManifest(t *testing.T) {
	store, dir := gcStore(t)
	ctx := t.Context()

	orphan := putBlob(t, store, "old orphan")
	makeOld(t, dir, orphan)
	if err := store.Put(ctx, "manifests/hosted/broken.json",
		strings.NewReader("{not json"), api.PutOpts{}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := sweep(ctx, store, nil, &out, discard(), true, 24*time.Hour)
	if err == nil {
		t.Fatal("sweep continued past a manifest it could not read")
	}
	if _, statErr := store.Stat(ctx, "blobs/sha256/"+orphan); statErr != nil {
		t.Error("a blob was deleted even though the mark phase failed")
	}
}

// The mark phase must survive a manifest that raced with a delete.
func TestGCToleratesAVanishedManifest(t *testing.T) {
	store, _ := gcStore(t)
	ctx := t.Context()

	digest, err := manifestDigest(ctx, store, "manifests/hosted/never-existed")
	if err != nil {
		t.Fatalf("a missing manifest is not an error: %v", err)
	}
	if digest != "" {
		t.Errorf("a missing manifest yielded a digest: %q", digest)
	}
}

func TestParseManifestSHA(t *testing.T) {
	digest := strings.Repeat("a", 64)
	body := fmt.Sprintf(`{"sha256": %q, "size": 12}`, digest)
	got, err := parseManifestSHA([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if got != digest {
		t.Errorf("sha256 = %q", got)
	}
	if _, err := parseManifestSHA([]byte("not json")); err == nil {
		t.Error("malformed manifest accepted")
	}
}
