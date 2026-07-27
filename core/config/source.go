package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Source is where the configuration document lives. There is exactly one
// document and it is always replaced whole: partial in-place edits would
// make "what is deployed" unanswerable, which is the point of keeping
// configuration declarative (invariant 8).
//
// Two implementations: the file on disk (the default, and what a
// GitOps-delivered ConfigMap looks like) and an object in the blob store
// (which every replica can read AND write, so the admin API works with N
// stateless replicas). Neither is the database — configuration stays out of
// it, so a site keeps serving with its last valid snapshot when PostgreSQL
// is down.
type Source interface {
	// Read returns the raw document and its version. A missing document is
	// ErrNoDocument, which the caller may treat as "seed me".
	Read(ctx context.Context) (raw []byte, version string, err error)
	// Write replaces the document. ifMatch, when non-empty, must equal the
	// current version or the write fails with ErrVersionConflict — that is
	// how two operators (or an operator and Terraform) are kept from
	// silently overwriting each other.
	Write(ctx context.Context, raw []byte, ifMatch string) (version string, err error)
	// Describe names the source for logs and errors.
	Describe() string
	// Writable reports whether Write is supported at all.
	Writable() bool
}

// ErrNoDocument means the source holds no configuration yet.
var ErrNoDocument = errors.New("no configuration document")

// ErrVersionConflict means the document changed since the caller read it.
var ErrVersionConflict = errors.New("configuration changed since it was read")

// ErrReadOnlySource means the configured source cannot be written through
// the API (a file mounted read-only, which is the norm in Kubernetes).
var ErrReadOnlySource = errors.New("configuration source is read-only")

// Version is the content hash of a document. Using the hash rather than a
// counter means two replicas that read the same bytes agree on the version
// without coordinating, and a write that changes nothing is detectable.
func Version(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// File source

// FileSource reads (and, when the file is writable, writes) the document on
// local disk.
type FileSource struct {
	path string
}

// NewFileSource builds a file-backed source.
func NewFileSource(path string) *FileSource { return &FileSource{path: path} }

// Read implements Source.
func (f *FileSource) Read(context.Context) ([]byte, string, error) {
	raw, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", fmt.Errorf("%s: %w", f.path, ErrNoDocument)
	}
	if err != nil {
		return nil, "", fmt.Errorf("read config %s: %w", f.path, err)
	}
	return raw, Version(raw), nil
}

// Write implements Source. It replaces the file atomically, so a reader
// that catches the moment sees the old document or the new one, never half.
func (f *FileSource) Write(ctx context.Context, raw []byte, ifMatch string) (string, error) {
	if ifMatch != "" {
		_, current, err := f.Read(ctx)
		if err != nil && !errors.Is(err, ErrNoDocument) {
			return "", err
		}
		if current != ifMatch {
			return "", fmt.Errorf("%w (have %s, expected %s)", ErrVersionConflict, current, ifMatch)
		}
	}

	dir := filepath.Dir(f.path)
	tmp, err := os.CreateTemp(dir, ".config-*.yaml")
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrReadOnlySource, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("sync config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close config: %w", err)
	}
	if err := os.Rename(tmpName, f.path); err != nil {
		return "", fmt.Errorf("%w: %v", ErrReadOnlySource, err)
	}
	return Version(raw), nil
}

// Describe implements Source.
func (f *FileSource) Describe() string { return "file " + f.path }

// Writable implements Source. A file mounted read-only (a ConfigMap) fails
// at write time with ErrReadOnlySource; probing here would be a lie the
// moment the mount changes, so this reports optimistically and the error
// path is precise.
func (f *FileSource) Writable() bool { return true }

// ---------------------------------------------------------------------------
// Blob-store source

// ObjectStore is the slice of the blob store a config source needs. It is
// declared here rather than imported so core/config keeps no dependency on
// core/api.
type ObjectStore interface {
	Get(ctx context.Context, key string) (io.ReadCloser, string, error)
	Put(ctx context.Context, key string, raw []byte, ifMatch string) (string, error)
}

// StoreSource keeps the document in the blob store, which is what lets an
// admin API work across N stateless replicas: every replica reads the same
// object and any replica can write it.
type StoreSource struct {
	store ObjectStore
	key   string
}

// ConfigObjectKey is where the document lives in the blob store. It sits
// outside blobs/ and manifests/ so garbage collection never looks at it.
const ConfigObjectKey = "config/registry.yaml"

// NewStoreSource builds a blob-store-backed source.
func NewStoreSource(store ObjectStore, key string) *StoreSource {
	if key == "" {
		key = ConfigObjectKey
	}
	return &StoreSource{store: store, key: key}
}

// Read implements Source.
func (s *StoreSource) Read(ctx context.Context) ([]byte, string, error) {
	rc, _, err := s.store.Get(ctx, s.key)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil, "", fmt.Errorf("%s: %w", s.key, ErrNoDocument)
		}
		return nil, "", fmt.Errorf("read config object %s: %w", s.key, err)
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(io.LimitReader(rc, 8<<20))
	if err != nil {
		return nil, "", fmt.Errorf("read config object %s: %w", s.key, err)
	}
	return raw, Version(raw), nil
}

// Write implements Source.
func (s *StoreSource) Write(ctx context.Context, raw []byte, ifMatch string) (string, error) {
	version, err := s.store.Put(ctx, s.key, raw, ifMatch)
	if err != nil {
		return "", err
	}
	return version, nil
}

// Describe implements Source.
func (s *StoreSource) Describe() string { return "object " + s.key }

// Writable implements Source.
func (s *StoreSource) Writable() bool { return true }
