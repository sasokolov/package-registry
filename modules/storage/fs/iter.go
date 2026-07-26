package fs

import (
	"context"
	"errors"
	"sync"

	"github.com/sasokolov/package-registry/core/api"
)

// chanIter adapts a channel-fed walk to api.Iter.
type chanIter struct {
	ch chan api.BlobInfo

	mu  sync.Mutex
	err error
}

func (it *chanIter) Next(ctx context.Context) (api.BlobInfo, bool) {
	select {
	case info, ok := <-it.ch:
		if !ok {
			return api.BlobInfo{}, false
		}
		return info, true
	case <-ctx.Done():
		it.setErr(ctx.Err())
		return api.BlobInfo{}, false
	}
}

func (it *chanIter) Err() error {
	it.mu.Lock()
	defer it.mu.Unlock()
	return it.err
}

func (it *chanIter) setErr(err error) {
	it.mu.Lock()
	defer it.mu.Unlock()
	if it.err == nil && err != nil && !errors.Is(err, context.Canceled) {
		it.err = err
	}
}
