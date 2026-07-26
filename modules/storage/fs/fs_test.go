package fs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/sasokolov/package-registry/core/api"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func sha256hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestPutGetStatRoundtrip(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	content := []byte("hello blob")
	key := "blobs/sha256/deadbeef"

	if err := s.Put(ctx, key, bytes.NewReader(content), api.PutOpts{SHA256: sha256hex(content), Size: int64(len(content))}); err != nil {
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
		t.Errorf("content = %q, want %q", got, content)
	}
	if info.Size != int64(len(content)) {
		t.Errorf("size = %d, want %d", info.Size, len(content))
	}
	if info.SHA256 != sha256hex(content) {
		t.Errorf("sha256 = %q, want %q", info.SHA256, sha256hex(content))
	}

	st, err := s.Stat(ctx, key)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.SHA256 != sha256hex(content) || st.Size != info.Size {
		t.Errorf("Stat = %+v, mismatch with Get info %+v", st, info)
	}
}

func TestPutChecksumMismatchLeavesNothing(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	key := "blobs/sha256/cafe"

	err := s.Put(ctx, key, strings.NewReader("real content"), api.PutOpts{SHA256: strings.Repeat("0", 64)})
	if !errors.Is(err, api.ErrChecksumMismatch) {
		t.Fatalf("Put = %v, want ErrChecksumMismatch", err)
	}
	if _, err := s.Stat(ctx, key); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("Stat after failed put = %v, want ErrNotFound", err)
	}
	// The staging area must not accumulate garbage.
	entries, err := os.ReadDir(filepath.Join(s.root, "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("tmp dir not empty after failed put: %v", entries)
	}
}

func TestPutSizeMismatch(t *testing.T) {
	s := newStore(t)
	if err := s.Put(t.Context(), "k", strings.NewReader("abc"), api.PutOpts{Size: 999}); err == nil {
		t.Fatal("Put with wrong size succeeded")
	}
	if _, err := s.Stat(t.Context(), "k"); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("Stat = %v, want ErrNotFound", err)
	}
}

func TestPutOverwriteMutable(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	key := "meta/feed/index.json"
	for _, body := range []string{"v1", "v2 longer body"} {
		if err := s.Put(ctx, key, strings.NewReader(body), api.PutOpts{}); err != nil {
			t.Fatalf("Put %q: %v", body, err)
		}
	}
	rc, info, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = rc.Close() }()
	b, _ := io.ReadAll(rc)
	if string(b) != "v2 longer body" {
		t.Errorf("content after overwrite = %q", b)
	}
	if info.SHA256 != sha256hex([]byte("v2 longer body")) {
		t.Errorf("attrs not updated on overwrite")
	}
}

func TestInvalidKeysRejected(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	for _, key := range []string{"", "/abs", "../escape", "a/../../b", "a/./b", "a//b", "trailing/", ".", ".."} {
		if err := s.Put(ctx, key, strings.NewReader("x"), api.PutOpts{}); err == nil {
			t.Errorf("Put(%q) succeeded, want error", key)
		}
		if _, err := s.Stat(ctx, key); err == nil {
			t.Errorf("Stat(%q) succeeded, want error", key)
		}
	}
	// Nothing may have escaped the root.
	if _, err := os.Stat(filepath.Join(filepath.Dir(s.root), "escape")); !errors.Is(err, os.ErrNotExist) {
		t.Error("traversal key escaped the storage root")
	}
}

func TestMissingBlob(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	if _, _, err := s.Get(ctx, "missing"); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("Get = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, "missing"); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("Delete = %v, want ErrNotFound", err)
	}
}

func TestDelete(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	if err := s.Put(ctx, "a/b", strings.NewReader("x"), api.PutOpts{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "a/b"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Stat(ctx, "a/b"); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("Stat after delete = %v, want ErrNotFound", err)
	}
}

func TestList(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	keys := []string{"manifests/f1/a", "manifests/f1/sub/b", "manifests/f2/c", "blobs/sha256/dd"}
	for _, k := range keys {
		if err := s.Put(ctx, k, strings.NewReader("x"), api.PutOpts{}); err != nil {
			t.Fatal(err)
		}
	}

	collect := func(prefix string) []string {
		t.Helper()
		it, err := s.List(ctx, prefix)
		if err != nil {
			t.Fatalf("List(%q): %v", prefix, err)
		}
		var got []string
		for {
			info, ok := it.Next(ctx)
			if !ok {
				break
			}
			got = append(got, info.Key)
		}
		if err := it.Err(); err != nil {
			t.Fatalf("iter error: %v", err)
		}
		sort.Strings(got)
		return got
	}

	if got := collect("manifests/f1/"); !equal(got, []string{"manifests/f1/a", "manifests/f1/sub/b"}) {
		t.Errorf("List(manifests/f1/) = %v", got)
	}
	if got := collect("manifests/"); len(got) != 3 {
		t.Errorf("List(manifests/) = %v, want 3 keys", got)
	}
	if got := collect("nope/"); len(got) != 0 {
		t.Errorf("List(nope/) = %v, want empty", got)
	}
}

func equal(a, b []string) bool {
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

func TestListHonorsContextCancel(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	for i := 0; i < 100; i++ {
		if err := s.Put(ctx, "many/"+strings.Repeat("x", i%10)+string(rune('a'+i%26))+
			"/"+hex.EncodeToString([]byte{byte(i)}), strings.NewReader("x"), api.PutOpts{}); err != nil {
			t.Fatal(err)
		}
	}
	listCtx, cancel := context.WithCancel(ctx)
	it, err := s.List(listCtx, "many/")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := it.Next(listCtx); !ok {
		t.Fatal("first Next returned no item")
	}
	cancel()
	// Iteration must terminate quickly after cancellation.
	for {
		if _, ok := it.Next(listCtx); !ok {
			break
		}
	}
}

func TestRegisteredFactory(t *testing.T) {
	store, err := api.NewStorage("fs", map[string]any{"path": t.TempDir()})
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	if _, ok := store.(*Store); !ok {
		t.Fatalf("NewStorage returned %T", store)
	}
	if _, err := api.NewStorage("fs", map[string]any{}); err == nil {
		t.Error("NewStorage without path succeeded")
	}
}
