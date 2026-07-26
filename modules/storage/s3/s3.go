// Package s3 implements api.BlobStore on any S3-compatible endpoint
// (AWS S3, MinIO, Ceph RGW) via minio-go. It also implements api.Presigner.
//
// Large uploads stream as multipart (fixed part size, unknown total length
// supported). The content digest is verified against PutOpts after upload
// and stored as object metadata; a mismatching object is removed.
package s3

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/sasokolov/package-registry/core/api"
)

// partSize is the multipart chunk size (S3 minimum is 5 MiB).
const partSize = 16 << 20

const sha256MetaKey = "Sha256" // stored as x-amz-meta-sha256

func init() {
	api.RegisterStorage("s3", func(options map[string]any) (api.BlobStore, error) {
		str := func(k string) string { v, _ := options[k].(string); return v }
		ssl, _ := options["use_ssl"].(bool)
		return New(Options{
			Endpoint:  str("endpoint"),
			Bucket:    str("bucket"),
			Region:    str("region"),
			AccessKey: str("access_key"),
			SecretKey: str("secret_key"),
			UseSSL:    ssl,
		})
	})
}

// Options configures the S3 store.
type Options struct {
	Endpoint  string
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
	UseSSL    bool
}

// Store is an S3-backed blob store.
type Store struct {
	client *minio.Client
	bucket string
}

// New builds the client. Bucket existence is ensured in Init (which needs
// a context); New itself performs no I/O.
func New(o Options) (*Store, error) {
	if o.Endpoint == "" || o.Bucket == "" {
		return nil, errors.New("s3 storage: endpoint and bucket are required")
	}
	client, err := minio.New(o.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(o.AccessKey, o.SecretKey, ""),
		Secure: o.UseSSL,
		Region: o.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("s3 storage: client: %w", err)
	}
	return &Store{client: client, bucket: o.Bucket}, nil
}

// Init implements api.Initializer: it creates the bucket if missing.
func (s *Store) Init(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("s3 storage: check bucket %s: %w", s.bucket, err)
	}
	if exists {
		return nil
	}
	err = s.client.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{})
	if err != nil {
		// Lost a create race with another replica: that is fine.
		if exists, err2 := s.client.BucketExists(ctx, s.bucket); err2 == nil && exists {
			return nil
		}
		return fmt.Errorf("s3 storage: create bucket %s: %w", s.bucket, err)
	}
	return nil
}

func notFound(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.Code == "NoSuchKey" || resp.Code == "NoSuchBucket" || resp.StatusCode == http.StatusNotFound
}

func infoFromStat(key string, st minio.ObjectInfo) api.BlobInfo {
	digest := st.UserMetadata[sha256MetaKey]
	if digest == "" {
		// ListObjects(WithMetadata) returns raw header names, StatObject
		// returns stripped ones; accept both.
		digest = st.UserMetadata["X-Amz-Meta-"+sha256MetaKey]
	}
	return api.BlobInfo{
		Key:     key,
		Size:    st.Size,
		SHA256:  digest,
		ModTime: st.LastModified,
	}
}

// Stat returns object metadata.
func (s *Store) Stat(ctx context.Context, key string) (api.BlobInfo, error) {
	st, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if notFound(err) {
			return api.BlobInfo{}, api.NotFoundf("blob %s", key)
		}
		return api.BlobInfo{}, fmt.Errorf("stat blob %s: %w", key, err)
	}
	return infoFromStat(key, st), nil
}

// Get opens the object for reading.
func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, api.BlobInfo, error) {
	info, err := s.Stat(ctx, key)
	if err != nil {
		return nil, api.BlobInfo{}, err
	}
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, api.BlobInfo{}, fmt.Errorf("get blob %s: %w", key, err)
	}
	return obj, info, nil
}

// Put streams the content to S3 (multipart for large/unknown sizes),
// verifying it against opts. S3 uploads are atomic: the object appears only
// once complete. If post-upload digest verification fails, the object is
// removed and ErrChecksumMismatch returned (defense in depth — the pipeline
// verifies checksums before calling Put).
func (s *Store) Put(ctx context.Context, key string, r io.Reader, opts api.PutOpts) error {
	h := sha256.New()
	tee := io.TeeReader(r, h)

	size := opts.Size
	if size <= 0 {
		size = -1 // unknown: minio-go switches to streaming multipart
	}
	putOpts := minio.PutObjectOptions{
		PartSize:    partSize,
		ContentType: "application/octet-stream",
	}
	uploaded, err := s.client.PutObject(ctx, s.bucket, key, tee, size, putOpts)
	if err != nil {
		return fmt.Errorf("put blob %s: %w", key, err)
	}
	if opts.Size > 0 && uploaded.Size != opts.Size {
		_ = s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
		return fmt.Errorf("put blob %s: size %d does not match expected %d", key, uploaded.Size, opts.Size)
	}
	digest := hex.EncodeToString(h.Sum(nil))
	if opts.SHA256 != "" && !strings.EqualFold(opts.SHA256, digest) {
		_ = s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
		return fmt.Errorf("put blob %s: got sha256 %s, want %s: %w", key, digest, opts.SHA256, api.ErrChecksumMismatch)
	}

	// Attach the digest as metadata. S3 metadata is immutable, so this is a
	// server-side copy; skip silently is not an option — Stat contract
	// promises the digest when known.
	_, err = s.client.CopyObject(ctx,
		minio.CopyDestOptions{
			Bucket:          s.bucket,
			Object:          key,
			UserMetadata:    map[string]string{sha256MetaKey: digest},
			ReplaceMetadata: true,
		},
		minio.CopySrcOptions{Bucket: s.bucket, Object: key},
	)
	if err != nil {
		return fmt.Errorf("put blob %s: attach digest metadata: %w", key, err)
	}
	return nil
}

// Delete removes the object; a missing object yields ErrNotFound.
func (s *Store) Delete(ctx context.Context, key string) error {
	if _, err := s.Stat(ctx, key); err != nil {
		return err
	}
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("delete blob %s: %w", key, err)
	}
	return nil
}

// List streams objects under prefix.
func (s *Store) List(ctx context.Context, prefix string) (api.Iter[api.BlobInfo], error) {
	ch := s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix:       prefix,
		Recursive:    true,
		WithMetadata: true,
	})
	return &objIter{ch: ch}, nil
}

type objIter struct {
	ch  <-chan minio.ObjectInfo
	err error
}

func (it *objIter) Next(ctx context.Context) (api.BlobInfo, bool) {
	select {
	case obj, ok := <-it.ch:
		if !ok {
			return api.BlobInfo{}, false
		}
		if obj.Err != nil {
			it.err = obj.Err
			return api.BlobInfo{}, false
		}
		return infoFromStat(obj.Key, obj), true
	case <-ctx.Done():
		return api.BlobInfo{}, false
	}
}

func (it *objIter) Err() error { return it.err }

// PresignGet implements api.Presigner.
func (s *Store) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	u, err := s.client.PresignedGetObject(ctx, s.bucket, key, ttl, nil)
	if err != nil {
		return "", fmt.Errorf("presign %s: %w", key, err)
	}
	return u.String(), nil
}
