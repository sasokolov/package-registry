package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/sasokolov/package-registry/core/api"
)

// blobObjectStore adapts a BlobStore to the ObjectStore a StoreSource needs.
//
// The conditional write is a read-compare-write, not an S3 precondition:
// object stores differ in what they support, and the admin API already
// holds a cross-replica advisory lock for the whole read-modify-write
// (invariant 9), which is what actually makes it atomic across replicas.
// The comparison here is the second line of defence, and the only one when
// no database is configured — which is why the admin API refuses to write
// without one.
type blobObjectStore struct {
	store api.BlobStore
}

// NewBlobStoreSource builds a config source backed by the blob store.
func NewBlobStoreSource(store api.BlobStore, key string) *StoreSource {
	return NewStoreSource(&blobObjectStore{store: store}, key)
}

func (b *blobObjectStore) Get(ctx context.Context, key string) (io.ReadCloser, string, error) {
	rc, info, err := b.store.Get(ctx, key)
	if err != nil {
		if errors.Is(err, api.ErrNotFound) {
			return nil, "", fmt.Errorf("%s: not found", key)
		}
		return nil, "", err
	}
	return rc, info.SHA256, nil
}

func (b *blobObjectStore) Put(ctx context.Context, key string, raw []byte, ifMatch string) (string, error) {
	if ifMatch != "" {
		current, _, err := b.readVersion(ctx, key)
		if err != nil && !errors.Is(err, ErrNoDocument) {
			return "", err
		}
		if current != ifMatch {
			return "", fmt.Errorf("%w (have %s, expected %s)", ErrVersionConflict, current, ifMatch)
		}
	}
	if err := b.store.Put(ctx, key, bytes.NewReader(raw), api.PutOpts{Size: int64(len(raw))}); err != nil {
		return "", fmt.Errorf("write config object %s: %w", key, err)
	}
	return Version(raw), nil
}

// readVersion returns the stored document's version.
func (b *blobObjectStore) readVersion(ctx context.Context, key string) (string, []byte, error) {
	rc, _, err := b.store.Get(ctx, key)
	if err != nil {
		if errors.Is(err, api.ErrNotFound) {
			return "", nil, ErrNoDocument
		}
		return "", nil, err
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(io.LimitReader(rc, 8<<20))
	if err != nil {
		return "", nil, err
	}
	return Version(raw), raw, nil
}
