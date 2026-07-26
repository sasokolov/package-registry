package repl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sasokolov/package-registry/core/api"
	"github.com/sasokolov/package-registry/modules/storage/fs"
)

// flakyPeer serves a blob but cuts the first transfer short, so the client
// has to resume with a Range request.
type flakyPeer struct {
	content []byte
	cut     int // bytes to serve before dropping the first response
	served  int
	ranges  []string
}

func (p *flakyPeer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.ranges = append(p.ranges, r.Header.Get("Range"))
	start := 0
	if spec := r.Header.Get("Range"); spec != "" {
		if n, ok := parseResumeOffset(spec); ok {
			start = int(n)
		}
	}
	body := p.content[start:]
	if start > 0 {
		w.WriteHeader(http.StatusPartialContent)
	}
	p.served++
	if p.served == 1 && p.cut > 0 && p.cut < len(body) {
		// Serve a prefix and hang up mid-body.
		_, _ = w.Write(body[:p.cut])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}
		return
	}
	_, _ = w.Write(body)
}

func TestFetchBlobResumesAfterInterruption(t *testing.T) {
	content := []byte(strings.Repeat("geo-replicated payload;", 500))
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])

	peer := &flakyPeer{content: content, cut: 1000}
	srv := httptest.NewServer(http.StripPrefix(InternalPrefix+"/blobs/sha256/"+digest, peer))
	defer srv.Close()

	store, err := fs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(Peer{Name: "peer-a", URL: srv.URL}, srv.Client(), nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := client.FetchBlob(t.Context(), store, digest, int64(len(content))); err != nil {
		t.Fatalf("FetchBlob: %v", err)
	}
	if len(peer.ranges) < 2 || peer.ranges[1] == "" {
		t.Fatalf("transfer was restarted rather than resumed: ranges=%v", peer.ranges)
	}

	rc, info, err := store.Get(t.Context(), "blobs/sha256/"+digest)
	if err != nil {
		t.Fatalf("stored blob: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, _ := io.ReadAll(rc)
	if string(got) != string(content) {
		t.Errorf("stored %d bytes, want %d", len(got), len(content))
	}
	if info.Size != int64(len(content)) {
		t.Errorf("stored size = %d", info.Size)
	}
}

// A peer that serves the wrong bytes must never have them stored: the key
// is the checksum (invariant 5).
func TestFetchBlobRejectsWrongContent(t *testing.T) {
	sum := sha256.Sum256([]byte("expected"))
	digest := hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("tampered"))
	}))
	defer srv.Close()

	store, err := fs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(Peer{Name: "hostile", URL: srv.URL}, srv.Client(), nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	err = client.FetchBlob(context.Background(), store, digest, 0)
	if !errors.Is(err, api.ErrChecksumMismatch) {
		t.Fatalf("FetchBlob = %v, want ErrChecksumMismatch", err)
	}
	if _, err := store.Stat(context.Background(), "blobs/sha256/"+digest); !errors.Is(err, api.ErrNotFound) {
		t.Error("tampered blob was stored")
	}
}
