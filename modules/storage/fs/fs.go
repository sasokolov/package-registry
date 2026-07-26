// Package fs implements api.BlobStore on top of a local directory.
//
// Layout under the root:
//
//	data/<key>        blob content
//	attrs/<key>.json  sidecar with the content digest
//	tmp/              staging area for atomic writes (same filesystem)
//
// Writes stream into tmp/ while hashing, are verified against PutOpts and
// then renamed into place, so readers never observe partial content.
package fs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/sasokolov/package-registry/core/api"
)

func init() {
	api.RegisterStorage("fs", func(options map[string]any) (api.BlobStore, error) {
		p, _ := options["path"].(string)
		return New(p)
	})
}

// Store is a directory-backed blob store.
type Store struct {
	root string
}

// New creates (if needed) the directory layout under root.
func New(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("fs storage: path is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("fs storage: resolve %s: %w", root, err)
	}
	for _, dir := range []string{"data", "attrs", "tmp"} {
		if err := os.MkdirAll(filepath.Join(abs, dir), 0o755); err != nil {
			return nil, fmt.Errorf("fs storage: create %s: %w", dir, err)
		}
	}
	return &Store{root: abs}, nil
}

type attrs struct {
	SHA256 string `json:"sha256"`
}

// keyPath validates a key and maps it into the given subtree.
func (s *Store) keyPath(tree, key string) (string, error) {
	if key == "" || strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") {
		return "", fmt.Errorf("invalid blob key %q", key)
	}
	if clean := path.Clean(key); clean != key || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("invalid blob key %q", key)
	}
	return filepath.Join(s.root, tree, filepath.FromSlash(key)), nil
}

// Get opens the blob for reading.
func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, api.BlobInfo, error) {
	info, err := s.Stat(ctx, key)
	if err != nil {
		return nil, api.BlobInfo{}, err
	}
	p, err := s.keyPath("data", key)
	if err != nil {
		return nil, api.BlobInfo{}, err
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, api.BlobInfo{}, api.NotFoundf("blob %s", key)
		}
		return nil, api.BlobInfo{}, fmt.Errorf("open blob %s: %w", key, err)
	}
	return f, info, nil
}

// Stat returns blob metadata.
func (s *Store) Stat(_ context.Context, key string) (api.BlobInfo, error) {
	p, err := s.keyPath("data", key)
	if err != nil {
		return api.BlobInfo{}, err
	}
	st, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return api.BlobInfo{}, api.NotFoundf("blob %s", key)
		}
		return api.BlobInfo{}, fmt.Errorf("stat blob %s: %w", key, err)
	}
	if st.IsDir() {
		return api.BlobInfo{}, api.NotFoundf("blob %s", key)
	}
	info := api.BlobInfo{Key: key, Size: st.Size(), ModTime: st.ModTime()}
	if a, err := s.readAttrs(key); err == nil {
		info.SHA256 = a.SHA256
	}
	return info, nil
}

func (s *Store) readAttrs(key string) (attrs, error) {
	p, err := s.keyPath("attrs", key+".json")
	if err != nil {
		return attrs{}, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return attrs{}, err
	}
	var a attrs
	if err := json.Unmarshal(b, &a); err != nil {
		return attrs{}, err
	}
	return a, nil
}

// Put atomically writes the blob: stream to tmp while hashing, verify
// against opts, then rename into place. On any failure nothing becomes
// visible under the key.
func (s *Store) Put(_ context.Context, key string, r io.Reader, opts api.PutOpts) error {
	dst, err := s.keyPath("data", key)
	if err != nil {
		return err
	}
	attrsDst, err := s.keyPath("attrs", key+".json")
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "put-*")
	if err != nil {
		return fmt.Errorf("put %s: create temp: %w", key, err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName) // no-op after successful rename
	}()

	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		return fmt.Errorf("put %s: write: %w", key, err)
	}
	if opts.Size > 0 && size != opts.Size {
		return fmt.Errorf("put %s: size %d does not match expected %d", key, size, opts.Size)
	}
	digest := hex.EncodeToString(h.Sum(nil))
	if opts.SHA256 != "" && !strings.EqualFold(opts.SHA256, digest) {
		return fmt.Errorf("put %s: got sha256 %s, want %s: %w", key, digest, opts.SHA256, api.ErrChecksumMismatch)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("put %s: sync: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("put %s: close: %w", key, err)
	}

	// Attrs first: an attrs file without data is invisible, the reverse
	// would briefly hide the digest.
	if err := writeFileAtomic(filepath.Join(s.root, "tmp"), attrsDst, mustJSON(attrs{SHA256: digest})); err != nil {
		return fmt.Errorf("put %s: attrs: %w", key, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("put %s: mkdir: %w", key, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("put %s: rename: %w", key, err)
	}
	return nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // attrs struct is always marshalable
	}
	return b
}

func writeFileAtomic(tmpDir, dst string, content []byte) error {
	f, err := os.CreateTemp(tmpDir, "attrs-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() {
		_ = f.Close()
		_ = os.Remove(name)
	}()
	if _, err := f.Write(content); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.Rename(name, dst)
}

// Delete removes the blob; missing blobs yield ErrNotFound.
func (s *Store) Delete(_ context.Context, key string) error {
	p, err := s.keyPath("data", key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return api.NotFoundf("blob %s", key)
		}
		return fmt.Errorf("delete blob %s: %w", key, err)
	}
	if ap, err := s.keyPath("attrs", key+".json"); err == nil {
		_ = os.Remove(ap) // best effort; data is already gone
	}
	return nil
}

// List streams blobs whose keys start with prefix. Iteration is driven by
// the ctx passed to List; cancel it to release the walker.
func (s *Store) List(ctx context.Context, prefix string) (api.Iter[api.BlobInfo], error) {
	dataRoot := filepath.Join(s.root, "data")
	// Walk from the deepest directory implied by the prefix to avoid
	// scanning the whole tree.
	startDir := dataRoot
	if i := strings.LastIndex(prefix, "/"); i >= 0 {
		p, err := s.keyPath("data", strings.TrimSuffix(prefix[:i], "/"))
		if err == nil {
			startDir = p
		}
	}

	ch := make(chan api.BlobInfo, 64)
	it := &chanIter{ch: ch}
	go func() {
		defer close(ch)
		err := filepath.WalkDir(startDir, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil // empty prefix space
				}
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(dataRoot, p)
			if err != nil {
				return err
			}
			key := filepath.ToSlash(rel)
			if !strings.HasPrefix(key, prefix) {
				return nil
			}
			st, err := d.Info()
			if err != nil {
				return err
			}
			info := api.BlobInfo{Key: key, Size: st.Size(), ModTime: st.ModTime()}
			if a, aerr := s.readAttrs(key); aerr == nil {
				info.SHA256 = a.SHA256
			}
			select {
			case ch <- info:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		it.setErr(err)
	}()
	return it, nil
}
