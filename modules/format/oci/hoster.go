package oci

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/fondaco-dev/fondaco/core/api"
)

// Pushing an image.
//
// A push is a conversation, which is what makes this protocol different from
// every other write path here:
//
//	POST  /v2/{name}/blobs/uploads/            -> 202 Location: .../<uuid>
//	PATCH <location>                           -> 202 Range: 0-<n>
//	PUT   <location>?digest=sha256:...         -> 201 the blob is committed
//	PUT   /v2/{name}/manifests/{tag}           -> 201 the image is visible
//
// Every answer tells the client where the next request goes, so the module
// writes its own responses (api.PublishResponder).
//
// The session state is the staged bytes themselves, kept in the blob store
// under api.StagingPrefix (<feed>/<repo>/<uuid>/<offset>) and never in this
// process: any replica can continue an upload another replica started, and a
// restart costs nothing but the unfinished bytes (invariant 3). The offset a
// client is told to continue from is derived by listing what is staged, so
// the answer is a fact about storage rather than something remembered.
//
// Nothing becomes visible before its digest is verified. The blob is
// assembled, hashed and written to blobs/sha256/<hash>, and the coordinate
// is committed only after that (invariant 5, invariant 10).

// maxManifestSize bounds a manifest body. The spec allows a registry to cap
// this; a manifest is a list of digests and nothing here is close.
const maxManifestSize = 8 << 20

// partWidth zero-pads a part's offset so the lexicographic order the store
// lists keys in is the order the bytes go back together in.
const partWidth = 16

// PublishRoutes implements api.PublishRouter: the write methods of the
// protocol, including the two the default catch-all would miss.
func (Module) PublishRoutes() []api.Route {
	return []api.Route{
		{Method: http.MethodPost, Pattern: "/*"},
		{Method: http.MethodPatch, Pattern: "/*"},
		{Method: http.MethodPut, Pattern: "/*"},
		{Method: http.MethodDelete, Pattern: "/*"},
	}
}

// HandlePublish implements api.Hoster.
//
// It refuses, and that is the honest answer rather than a stub: every write
// in this protocol is answered with headers the next request depends on, so
// a caller that discards the response cannot carry out a push at all. The
// real implementation is HandlePublishHTTP, which the core uses because this
// module declares api.PublishResponder.
func (Module) HandlePublish(context.Context, api.Feed, *http.Request, api.CoreServices) error {
	return fmt.Errorf("a registry push is a conversation and needs its response: %w", api.ErrBadRequest)
}

// Reindex implements api.Hoster: there is nothing to generate.
//
// A tag is not a derived document but a published coordinate that points at
// a manifest, so it is already correct everywhere the coordinate is — after
// a local push and after a replicated one alike. Generating a copy of it
// would be a second thing to keep in step with the first, and the only way
// for a registry to contradict itself.
func (Module) Reindex(context.Context, api.Feed, api.CoreServices) error { return nil }

// HandlePublishHTTP implements api.PublishResponder.
func (m Module) HandlePublishHTTP(ctx context.Context, feed api.Feed, w http.ResponseWriter,
	r *http.Request, deps api.CoreServices) error {
	p := strings.TrimPrefix(r.URL.Path, "/")
	if strings.Contains(p, "..") {
		return api.NotFoundf("invalid path %q", p)
	}
	rest, ok := strings.CutPrefix(p, apiRoot+"/")
	if !ok {
		return api.NotFoundf("not a registry API path: %q", p)
	}
	ref, err := parseRef(rest)
	if err != nil {
		return err
	}

	switch r.Method {
	case http.MethodPost:
		if ref.kind != refUpload {
			return api.NotFoundf("nothing is written by POST at %q", p)
		}
		return m.startUpload(ctx, feed, w, r, deps, ref.repo)

	case http.MethodPatch:
		if ref.kind != refUpload || ref.reference == "" {
			return api.NotFoundf("not an upload session: %q", p)
		}
		return m.appendChunk(ctx, feed, w, r, deps, ref.repo, ref.reference)

	case http.MethodPut:
		switch ref.kind {
		case refUpload:
			if ref.reference == "" {
				return api.NotFoundf("not an upload session: %q", p)
			}
			return m.finishUpload(ctx, feed, w, r, deps, ref.repo, ref.reference)
		case refManifest:
			return m.putManifest(ctx, feed, w, r, deps, ref.repo, ref.reference)
		default:
			return api.NotFoundf("nothing is written by PUT at %q", p)
		}

	case http.MethodDelete:
		if ref.kind == refUpload && ref.reference != "" {
			// Cancelling an unfinished upload throws away staging, not
			// anything anybody could have pulled.
			if err := deleteParts(ctx, deps, uploadKeyPrefix(feed, ref.repo, ref.reference)); err != nil {
				return err
			}
			w.WriteHeader(http.StatusNoContent)
			return nil
		}
		return fmt.Errorf(
			"a published image is immutable; take it out of circulation with quarantine instead of deleting it: %w",
			api.ErrForbidden)

	default:
		return fmt.Errorf("method %s is not part of this protocol: %w", r.Method, api.ErrBadRequest)
	}
}

// ---------------------------------------------------------------------------
// Blob uploads

// startUpload answers POST /v2/{name}/blobs/uploads/.
//
// Three shapes arrive here: a plain session start, a monolithic upload with
// the whole blob and its digest, and a cross-repository mount. The mount is
// worth honouring rather than refusing — blobs are content addressed, so a
// layer this site already has needs no bytes at all, only a coordinate.
func (m Module) startUpload(ctx context.Context, feed api.Feed, w http.ResponseWriter,
	r *http.Request, deps api.CoreServices, repo string) error {
	query := r.URL.Query()

	if mount := query.Get("mount"); mount != "" {
		digest, ok := parseDigest(mount)
		if !ok {
			return fmt.Errorf("mount digest %q is not a digest: %w", mount, api.ErrBadRequest)
		}
		if info, err := deps.Blobs().Stat(ctx, blobStoreKey(digest.Hex)); err == nil {
			if err := publishBlob(ctx, feed, deps, repo, digest.Hex, info.Size); err != nil {
				return err
			}
			blobLocation(w, feed, repo, "sha256:"+digest.Hex)
			w.WriteHeader(http.StatusCreated)
			return nil
		}
		// Not here: fall through to a normal upload, which is what the spec
		// says a registry that cannot mount must do.
	}

	uploadID, err := newUploadID()
	if err != nil {
		return err
	}

	if digest := query.Get("digest"); digest != "" {
		// Monolithic: the whole blob is in this request.
		if _, err := stagePart(ctx, deps, feed, repo, uploadID, 0, r.Body); err != nil {
			return err
		}
		return m.commitUpload(ctx, feed, w, deps, repo, uploadID, digest)
	}

	uploadAccepted(w, feed, repo, uploadID, 0)
	return nil
}

// appendChunk answers PATCH <location>.
func (Module) appendChunk(ctx context.Context, feed api.Feed, w http.ResponseWriter,
	r *http.Request, deps api.CoreServices, repo, uploadID string) error {
	if err := validUploadID(uploadID); err != nil {
		return err
	}
	offset, err := stagedSize(ctx, deps, feed, repo, uploadID)
	if err != nil {
		return err
	}
	// A client that says where its chunk starts must agree with what is
	// already staged, or the blob would be assembled with a hole in it.
	if start, ok := rangeStart(r.Header.Get("Content-Range")); ok && start != offset {
		w.Header().Set("Range", fmt.Sprintf("0-%d", lastByte(offset)))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return nil
	}

	written, err := stagePart(ctx, deps, feed, repo, uploadID, offset, r.Body)
	if err != nil {
		return err
	}
	uploadAccepted(w, feed, repo, uploadID, offset+written)
	return nil
}

// finishUpload answers PUT <location>?digest=...
func (m Module) finishUpload(ctx context.Context, feed api.Feed, w http.ResponseWriter,
	r *http.Request, deps api.CoreServices, repo, uploadID string) error {
	if err := validUploadID(uploadID); err != nil {
		return err
	}
	digest := r.URL.Query().Get("digest")
	if digest == "" {
		return fmt.Errorf("a completed upload must name the digest it produced: %w", api.ErrBadRequest)
	}
	// The last chunk may travel with the PUT.
	offset, err := stagedSize(ctx, deps, feed, repo, uploadID)
	if err != nil {
		return err
	}
	if r.Body != nil {
		if _, err := stagePart(ctx, deps, feed, repo, uploadID, offset, r.Body); err != nil {
			return err
		}
	}
	return m.commitUpload(ctx, feed, w, deps, repo, uploadID, digest)
}

// commitUpload assembles what was staged, verifies it against the digest the
// client promised and publishes the blob.
func (Module) commitUpload(ctx context.Context, feed api.Feed, w http.ResponseWriter,
	deps api.CoreServices, repo, uploadID, digest string) error {
	want, ok := parseDigest(digest)
	if !ok {
		return fmt.Errorf("digest %q is not a digest: %w", digest, api.ErrBadRequest)
	}
	if want.Algo != "sha256" {
		return fmt.Errorf("this registry addresses content by sha256, not %s: %w", want.Algo, api.ErrBadRequest)
	}

	prefix := uploadKeyPrefix(feed, repo, uploadID)
	parts, total, err := stagedParts(ctx, deps, prefix)
	if err != nil {
		return err
	}
	if len(parts) == 0 {
		return fmt.Errorf("upload %s has nothing staged: %w", uploadID, api.ErrBadRequest)
	}

	// The store verifies the digest while writing and leaves nothing visible
	// on a mismatch, so a layer that arrived corrupted never exists.
	body := &partsReader{ctx: ctx, store: deps.Blobs(), keys: parts}
	defer func() { _ = body.Close() }()
	err = deps.Blobs().Put(ctx, blobStoreKey(want.Hex), body,
		api.PutOpts{SHA256: want.Hex, Size: total, ContentType: mediaTypeOctetStream})
	if err != nil {
		return fmt.Errorf("commit upload %s: %w", uploadID, err)
	}

	if err := publishBlob(ctx, feed, deps, repo, want.Hex, total); err != nil {
		return err
	}
	if err := deleteParts(ctx, deps, prefix); err != nil {
		return err
	}

	blobLocation(w, feed, repo, digest)
	w.WriteHeader(http.StatusCreated)
	return nil
}

// publishBlob commits the coordinate a blob is reachable at inside a feed.
//
// The bytes are shared — the same layer under ten images is one object — but
// being able to pull it from a repository is a per-repository fact, and it
// is the coordinate that policies, audit and replication see.
func publishBlob(ctx context.Context, feed api.Feed, deps api.CoreServices,
	repo, sha256hex string, size int64) error {
	_, err := deps.Publish(ctx, api.PublishRequest{
		Feed:      feed,
		Coord:     api.PackageCoordinate{Format: formatName, Name: repo},
		Path:      blobPath(repo, "sha256:"+sha256hex),
		SHA256:    sha256hex,
		Size:      size,
		Checksums: map[string]string{"sha256": sha256hex},
		Metadata:  map[string]string{api.MetaContentType: mediaTypeOctetStream},
	})
	if err != nil {
		return fmt.Errorf("publish blob sha256:%s: %w", sha256hex, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Manifests

// manifestDoc is as much of a manifest as this registry needs to understand:
// what kind of document it is and what it references.
type manifestDoc struct {
	MediaType string `json:"mediaType"`
	Config    struct {
		Digest string `json:"digest"`
	} `json:"config"`
	Layers []struct {
		Digest    string `json:"digest"`
		MediaType string `json:"mediaType"`
	} `json:"layers"`
	Manifests []struct {
		Digest string `json:"digest"`
	} `json:"manifests"`
}

// putManifest answers PUT /v2/{name}/manifests/{reference}.
func (Module) putManifest(ctx context.Context, feed api.Feed, w http.ResponseWriter,
	r *http.Request, deps api.CoreServices, repo, reference string) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxManifestSize+1))
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	if len(body) > maxManifestSize {
		return fmt.Errorf("manifest exceeds %d bytes: %w", maxManifestSize, api.ErrBadRequest)
	}

	var doc manifestDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("manifest is not a JSON document: %v: %w", err, api.ErrBadRequest)
	}
	mediaType := strings.TrimSpace(r.Header.Get("Content-Type"))
	if mediaType == "" {
		mediaType = doc.MediaType
	}
	if !acceptableMediaType(mediaType) {
		return fmt.Errorf("media type %q is not an image manifest or index: %w", mediaType, api.ErrBadRequest)
	}

	digest := sha256Hex(body)
	if want, isDigest := parseDigest(reference); isDigest {
		if want.Algo != "sha256" || !strings.EqualFold(want.Hex, digest) {
			return fmt.Errorf("manifest digest is sha256:%s, not %s: %w",
				digest, reference, api.ErrChecksumMismatch)
		}
	} else if !validTag(reference) {
		return fmt.Errorf("tag %q is not a tag: %w", reference, api.ErrBadRequest)
	}

	// An image that references bytes this feed does not have is not an
	// image; it is a manifest that would 404 halfway through every pull.
	if err := checkReferences(ctx, feed, deps, repo, doc); err != nil {
		return err
	}

	if err := deps.Blobs().Put(ctx, blobStoreKey(digest), strings.NewReader(string(body)),
		api.PutOpts{SHA256: digest, Size: int64(len(body)), ContentType: mediaType}); err != nil {
		return fmt.Errorf("store manifest: %w", err)
	}
	metadata := map[string]string{api.MetaContentType: mediaType}

	// The manifest at its digest is the release, and it is immutable.
	if _, err := deps.Publish(ctx, api.PublishRequest{
		Feed:      feed,
		Coord:     api.PackageCoordinate{Format: formatName, Name: repo, Version: "sha256:" + digest},
		Path:      manifestPath(repo, "sha256:"+digest),
		SHA256:    digest,
		Size:      int64(len(body)),
		Checksums: map[string]string{"sha256": digest},
		Metadata:  metadata,
	}); err != nil {
		return fmt.Errorf("publish manifest sha256:%s: %w", digest, err)
	}

	// The tag is a pointer to it, and pointers move: that is what a tag is
	// for, and refusing to move one would make this registry unusable for
	// images while protecting nothing — the release it pointed at is still
	// there, unchanged, at its digest (invariant 4).
	if _, isDigest := parseDigest(reference); !isDigest {
		if _, err := deps.Publish(ctx, api.PublishRequest{
			Feed:      feed,
			Coord:     api.PackageCoordinate{Format: formatName, Name: repo, Version: reference},
			Path:      manifestPath(repo, reference),
			SHA256:    digest,
			Size:      int64(len(body)),
			Checksums: map[string]string{"sha256": digest},
			Metadata:  metadata,
			Mutable:   true,
		}); err != nil {
			return fmt.Errorf("point %s:%s at sha256:%s: %w", repo, reference, digest, err)
		}
	}

	w.Header().Set("Location", feedURL(feed)+"/"+repo+"/manifests/sha256:"+digest)
	w.Header().Set("Docker-Content-Digest", "sha256:"+digest)
	w.WriteHeader(http.StatusCreated)
	return nil
}

// acceptableMediaType reports whether this registry stores a document of
// this type as a manifest. Schema 1 is refused: it is deprecated and its
// signatures make the same image different bytes every time.
func acceptableMediaType(mediaType string) bool {
	switch strings.SplitN(mediaType, ";", 2)[0] {
	case mediaTypeDockerManifest, mediaTypeDockerList, mediaTypeOCIManifest, mediaTypeOCIIndex:
		return true
	default:
		return false
	}
}

// checkReferences verifies that everything the manifest names is already
// published in this repository.
func checkReferences(ctx context.Context, feed api.Feed, deps api.CoreServices,
	repo string, doc manifestDoc) error {
	for _, digest := range referencedBlobs(doc) {
		if err := requirePublished(ctx, feed, deps, blobPath(repo, digest)); err != nil {
			return err
		}
	}
	for _, child := range doc.Manifests {
		if child.Digest == "" {
			continue
		}
		if err := requirePublished(ctx, feed, deps, manifestPath(repo, child.Digest)); err != nil {
			return err
		}
	}
	return nil
}

func referencedBlobs(doc manifestDoc) []string {
	var out []string
	if doc.Config.Digest != "" {
		out = append(out, doc.Config.Digest)
	}
	for _, layer := range doc.Layers {
		if layer.Digest == "" {
			continue
		}
		if strings.HasSuffix(layer.MediaType, ".foreign.diff.tar.gzip") {
			// A foreign layer is deliberately hosted elsewhere; the manifest
			// carries its URLs and no registry is expected to have it.
			continue
		}
		out = append(out, layer.Digest)
	}
	return out
}

func requirePublished(ctx context.Context, feed api.Feed, deps api.CoreServices, path string) error {
	manifests, err := deps.Manifests(ctx, feed, path)
	if err != nil {
		return fmt.Errorf("check %s: %w", path, err)
	}
	for _, m := range manifests {
		if m.Path == path {
			return nil
		}
	}
	return fmt.Errorf("the manifest references %s, which has not been uploaded: %w",
		strings.TrimPrefix(path, apiRoot+"/"), api.ErrBadRequest)
}

// ---------------------------------------------------------------------------
// Staging

func newUploadID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate an upload id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// validUploadID keeps a client-supplied session name from becoming a storage
// key of its own choosing.
func validUploadID(id string) error {
	if len(id) < 8 || len(id) > 64 {
		return api.NotFoundf("upload %q is not a session", id)
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') && c != '-' {
			return api.NotFoundf("upload %q is not a session", id)
		}
	}
	return nil
}

func uploadKeyPrefix(feed api.Feed, repo, uploadID string) string {
	return api.StagingPrefix + feed.Name + "/" + repo + "/" + uploadID + "/"
}

func blobStoreKey(sha256hex string) string { return "blobs/sha256/" + sha256hex }

// stagePart writes one chunk at its offset and reports how many bytes it
// held. The offset is in the key, so the parts of an upload sort back into
// the order they arrived in whoever lists them.
func stagePart(ctx context.Context, deps api.CoreServices, feed api.Feed,
	repo, uploadID string, offset int64, body io.Reader) (int64, error) {
	if body == nil {
		return 0, nil
	}
	counter := &countingReader{r: body}
	key := fmt.Sprintf("%s%0*d", uploadKeyPrefix(feed, repo, uploadID), partWidth, offset)
	if err := deps.Blobs().Put(ctx, key, counter, api.PutOpts{}); err != nil {
		return 0, fmt.Errorf("stage upload chunk: %w", err)
	}
	return counter.n, nil
}

// stagedParts lists an upload's chunks in order and their total size.
func stagedParts(ctx context.Context, deps api.CoreServices, prefix string) ([]string, int64, error) {
	iter, err := deps.Blobs().List(ctx, prefix)
	if err != nil {
		return nil, 0, fmt.Errorf("list staged chunks: %w", err)
	}
	var keys []string
	var total int64
	for {
		info, ok := iter.Next(ctx)
		if !ok {
			break
		}
		keys = append(keys, info.Key)
		total += info.Size
	}
	if err := iter.Err(); err != nil {
		return nil, 0, fmt.Errorf("list staged chunks: %w", err)
	}
	return keys, total, nil
}

// stagedSize is how far an upload has got, which is what a client is told to
// continue from. It is read from storage rather than remembered, so any
// replica can answer it (invariant 3).
func stagedSize(ctx context.Context, deps api.CoreServices, feed api.Feed, repo, uploadID string) (int64, error) {
	_, total, err := stagedParts(ctx, deps, uploadKeyPrefix(feed, repo, uploadID))
	return total, err
}

func deleteParts(ctx context.Context, deps api.CoreServices, prefix string) error {
	keys, _, err := stagedParts(ctx, deps, prefix)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if err := deps.Blobs().Delete(ctx, key); err != nil {
			return fmt.Errorf("discard staged chunk: %w", err)
		}
	}
	return nil
}

// partsReader reads an upload's chunks back as one stream.
type partsReader struct {
	//nolint:containedctx // an io.Reader has nowhere else to carry it
	ctx     context.Context
	store   api.BlobStore
	keys    []string
	current io.ReadCloser
	next    int
}

func (p *partsReader) Read(b []byte) (int, error) {
	for {
		if p.current == nil {
			if p.next >= len(p.keys) {
				return 0, io.EOF
			}
			rc, _, err := p.store.Get(p.ctx, p.keys[p.next])
			if err != nil {
				return 0, fmt.Errorf("read staged chunk: %w", err)
			}
			p.current = rc
			p.next++
		}
		n, err := p.current.Read(b)
		if n > 0 {
			return n, nil
		}
		if err == io.EOF {
			_ = p.current.Close()
			p.current = nil
			continue
		}
		if err != nil {
			return 0, err
		}
	}
}

func (p *partsReader) Close() error {
	if p.current != nil {
		err := p.current.Close()
		p.current = nil
		return err
	}
	return nil
}

// countingReader remembers how much passed through it, so a chunk written
// with an unknown length still reports its size.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n += int64(n)
	return n, err
}

// ---------------------------------------------------------------------------
// Responses

// uploadAccepted is the answer that keeps a session going: where to send the
// next chunk and how much has arrived.
func uploadAccepted(w http.ResponseWriter, feed api.Feed, repo, uploadID string, written int64) {
	location := feedURL(feed) + "/" + repo + "/blobs/uploads/" + uploadID
	w.Header().Set("Location", location)
	w.Header().Set("Docker-Upload-UUID", uploadID)
	w.Header().Set("Range", fmt.Sprintf("0-%d", lastByte(written)))
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusAccepted)
}

func blobLocation(w http.ResponseWriter, feed api.Feed, repo, digest string) {
	w.Header().Set("Location", feedURL(feed)+"/"+repo+"/blobs/"+digest)
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Length", "0")
}

// lastByte turns a count into the inclusive end of a byte range, which is
// how this protocol reports progress. Nothing staged is reported as 0-0,
// which is what the spec's own examples do.
func lastByte(written int64) int64 {
	if written <= 0 {
		return 0
	}
	return written - 1
}

// rangeStart reads the first byte offset out of a Content-Range header.
func rangeStart(header string) (int64, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, false
	}
	// Both "start-end" and "bytes start-end" are seen in the wild.
	header = strings.TrimPrefix(header, "bytes ")
	start, _, ok := strings.Cut(header, "-")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(start), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
