//go:build integration

// Integration tests against a real MinIO from the compose stack:
//
//	make test-integration
//
// Required env: S3_TEST_ENDPOINT, S3_TEST_ACCESS_KEY, S3_TEST_SECRET_KEY.
package s3

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sasokolov/package-registry/core/api"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	endpoint := os.Getenv("S3_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("S3_TEST_ENDPOINT not set; run via make test-integration")
	}
	s, err := New(Options{
		Endpoint:  endpoint,
		Bucket:    fmt.Sprintf("it-%d", time.Now().UnixNano()),
		AccessKey: os.Getenv("S3_TEST_ACCESS_KEY"),
		SecretKey: os.Getenv("S3_TEST_SECRET_KEY"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Init(t.Context()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return s
}

func TestRoundtripAndStat(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	content := []byte("s3 blob content")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	key := "blobs/sha256/" + digest

	if err := s.Put(ctx, key, bytes.NewReader(content), api.PutOpts{SHA256: digest, Size: int64(len(content))}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, info, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Error("content mismatch")
	}
	if info.SHA256 != digest {
		t.Errorf("stat sha256 = %q, want %q", info.SHA256, digest)
	}
	if info.Size != int64(len(content)) {
		t.Errorf("size = %d", info.Size)
	}
}

func TestChecksumMismatchRemovesObject(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	err := s.Put(ctx, "bad/blob", strings.NewReader("content"), api.PutOpts{SHA256: strings.Repeat("0", 64)})
	if !errors.Is(err, api.ErrChecksumMismatch) {
		t.Fatalf("Put = %v, want ErrChecksumMismatch", err)
	}
	if _, err := s.Stat(ctx, "bad/blob"); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("Stat after failed put = %v, want ErrNotFound", err)
	}
}

func TestMultipartLargeBlob(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	// 3 parts at the 16 MiB PartSize boundary would need 48 MiB; use
	// unknown size to force the streaming-multipart code path instead.
	big := make([]byte, 18<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(big)
	digest := hex.EncodeToString(sum[:])

	// Size unknown (0): minio-go streams in PartSize chunks.
	if err := s.Put(ctx, "big/blob", bytes.NewReader(big), api.PutOpts{SHA256: digest}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	info, err := s.Stat(ctx, "big/blob")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != int64(len(big)) {
		t.Errorf("size = %d, want %d", info.Size, len(big))
	}
	if info.SHA256 != digest {
		t.Errorf("sha256 metadata lost on multipart upload")
	}
}

func TestPresignGet(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	content := []byte("presigned content")
	if err := s.Put(ctx, "presign/blob", bytes.NewReader(content), api.PutOpts{}); err != nil {
		t.Fatal(err)
	}
	url, err := s.PresignGet(ctx, "presign/blob", time.Minute)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET presigned: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("presigned GET status = %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, content) {
		t.Error("presigned content mismatch")
	}
}

func TestListAndDelete(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	for _, k := range []string{"list/a", "list/sub/b", "other/c"} {
		if err := s.Put(ctx, k, strings.NewReader("x"), api.PutOpts{}); err != nil {
			t.Fatal(err)
		}
	}
	it, err := s.List(ctx, "list/")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for {
		info, ok := it.Next(ctx)
		if !ok {
			break
		}
		keys = append(keys, info.Key)
	}
	if err := it.Err(); err != nil {
		t.Fatalf("iter: %v", err)
	}
	if len(keys) != 2 {
		t.Errorf("List = %v, want 2 keys", keys)
	}

	if err := s.Delete(ctx, "list/a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete(ctx, "list/a"); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("second Delete = %v, want ErrNotFound", err)
	}
}
