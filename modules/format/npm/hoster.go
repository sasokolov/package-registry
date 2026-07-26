package npm

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // npm's legacy dist.shasum
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/sasokolov/package-registry/core/api"
)

// maxPublishSize bounds a publish body (JSON with base64 attachments).
const maxPublishSize = 512 << 20

// publishDoc is the document `npm publish` PUTs to /{pkg}.
type publishDoc struct {
	Name        string            `json:"name"`
	DistTags    map[string]string `json:"dist-tags"`
	Versions    map[string]any    `json:"versions"`
	Attachments map[string]struct {
		ContentType string `json:"content_type"`
		Data        string `json:"data"`
		Length      int64  `json:"length"`
	} `json:"_attachments"`
	// Set by `npm unpublish`, which republishes the document without the
	// removed version.
	Revision string `json:"_rev"`
}

// PublishRoutes implements api.PublishRouter: npm publishes with
// PUT /{pkg} (and PUT /{pkg}/-rev/{rev} for unpublish).
func (Module) PublishRoutes() []api.Route {
	return []api.Route{
		{Method: http.MethodPut, Pattern: "/*"},
		{Method: http.MethodPost, Pattern: "/*"},
		{Method: http.MethodDelete, Pattern: "/*"},
	}
}

// HandlePublish implements api.Hoster.
//
//   - `npm publish` PUTs the package document with the tarball inline as a
//     base64 attachment: the tarball becomes an immutable coordinate and the
//     dist-tags are updated;
//   - `npm unpublish` PUTs the document without the version (or DELETEs the
//     tarball): the version is hidden from the index, the blob is kept
//     (invariant 10 — content-addressed blobs are shared and GC'd separately).
func (Module) HandlePublish(ctx context.Context, feed api.Feed, r *http.Request, deps api.CoreServices) error {
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		return fmt.Errorf("empty publish path: %w", api.ErrBadRequest)
	}
	if r.Method == http.MethodDelete {
		// Tarball deletion during unpublish: the index is rebuilt from the
		// remaining versions, blobs are never removed here.
		return nil
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxPublishSize))
	if err != nil {
		return fmt.Errorf("read publish document: %w", err)
	}
	var doc publishDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("parse publish document: %v: %w", err, api.ErrBadRequest)
	}
	if doc.Name == "" {
		return fmt.Errorf("publish document has no package name: %w", api.ErrBadRequest)
	}

	if len(doc.Attachments) == 0 {
		// No attachment: this is an unpublish or a dist-tag-only update.
		return publishState(ctx, feed, doc, deps)
	}

	for filename, attachment := range doc.Attachments {
		raw, err := base64.StdEncoding.DecodeString(attachment.Data)
		if err != nil {
			return fmt.Errorf("attachment %s is not valid base64: %w", filename, api.ErrBadRequest)
		}
		version := versionFromTarball(doc.Name, filename)
		if version == "" {
			return fmt.Errorf("cannot derive a version from attachment %q: %w", filename, api.ErrBadRequest)
		}
		digests := digestsOf(raw)
		sha256hex := digests["sha256"]
		if err := deps.Blobs().Put(ctx, "blobs/sha256/"+sha256hex, bytes.NewReader(raw),
			api.PutOpts{SHA256: sha256hex, Size: int64(len(raw))}); err != nil {
			return fmt.Errorf("stage tarball: %w", err)
		}

		meta := map[string]string{api.MetaEcosystem: "npm"}
		if v, ok := doc.Versions[version].(map[string]any); ok {
			if lic := licenseString(v["license"]); lic != "" {
				meta[api.MetaLicense] = lic
			}
		}

		if _, err := deps.Publish(ctx, api.PublishRequest{
			Feed:      feed,
			Coord:     api.PackageCoordinate{Format: "npm", Name: doc.Name, Version: version},
			Path:      tarballPath(doc.Name, filename),
			SHA256:    sha256hex,
			Size:      int64(len(raw)),
			Checksums: digests,
			Metadata:  meta,
		}); err != nil {
			return err
		}
	}
	return publishState(ctx, feed, doc, deps)
}

// publishState stores the per-version documents and dist-tag pointers that
// Reindex assembles the package root from. They are mutable coordinates:
// dist-tags move, and npm re-publishes the whole document on every change.
func publishState(ctx context.Context, feed api.Feed, doc publishDoc, deps api.CoreServices) error {
	for version, raw := range doc.Versions {
		encoded, err := json.Marshal(raw)
		if err != nil {
			return fmt.Errorf("encode version %s: %w", version, err)
		}
		if err := putMutable(ctx, feed, deps,
			versionDocPath(doc.Name, version),
			api.PackageCoordinate{Format: "npm", Name: doc.Name, Version: version},
			encoded); err != nil {
			return err
		}
	}
	for tag, version := range doc.DistTags {
		if tag == "" || version == "" {
			continue
		}
		// Each tag is its own mutable pointer, so concurrent tag moves do
		// not overwrite each other (and geo replication merges per tag).
		if err := putMutable(ctx, feed, deps,
			distTagPath(doc.Name, tag),
			api.PackageCoordinate{Format: "npm", Name: doc.Name},
			[]byte(version)); err != nil {
			return err
		}
	}
	return nil
}

// putMutable stages a small document as a blob and publishes it as a
// mutable coordinate.
func putMutable(ctx context.Context, feed api.Feed, deps api.CoreServices,
	path string, coord api.PackageCoordinate, body []byte) error {
	digests := digestsOf(body)
	sha256hex := digests["sha256"]
	if err := deps.Blobs().Put(ctx, "blobs/sha256/"+sha256hex, bytes.NewReader(body),
		api.PutOpts{SHA256: sha256hex, Size: int64(len(body))}); err != nil {
		return fmt.Errorf("stage %s: %w", path, err)
	}
	_, err := deps.Publish(ctx, api.PublishRequest{
		Feed:      feed,
		Coord:     coord,
		Path:      path,
		SHA256:    sha256hex,
		Size:      int64(len(body)),
		Checksums: digests,
		Mutable:   true,
		Metadata:  map[string]string{api.MetaEcosystem: "npm"},
	})
	return err
}

// Storage layout of a hosted npm feed. Tarballs keep the protocol path so
// they are served directly; the rest lives under a reserved prefix that no
// npm request can address (the "-" segment is protocol-reserved).
func tarballPath(pkg, filename string) string { return pkg + "/-/" + filename }
func versionDocPath(pkg, version string) string {
	return "-/hosted/" + pkg + "/versions/" + version + ".json"
}
func distTagPath(pkg, tag string) string { return "-/hosted/" + pkg + "/dist-tags/" + tag }

func digestsOf(body []byte) map[string]string {
	sha1sum := sha1.Sum(body) //nolint:gosec // npm's legacy dist.shasum
	sha256sum := sha256.Sum256(body)
	sha512sum := sha512.Sum512(body)
	return map[string]string{
		"sha1":   hex.EncodeToString(sha1sum[:]),
		"sha256": hex.EncodeToString(sha256sum[:]),
		"sha512": hex.EncodeToString(sha512sum[:]),
	}
}

// Reindex implements api.Hoster: rebuild each package's root document and
// dist-tags from the published coordinates. Deterministic by construction:
// the same manifest set always yields the same bytes.
func (Module) Reindex(ctx context.Context, feed api.Feed, deps api.CoreServices) error {
	manifests, err := deps.Manifests(ctx, feed, "")
	if err != nil {
		return err
	}

	type pkgState struct {
		versions map[string]json.RawMessage
		tarballs map[string]api.HostedManifest
		tags     map[string]string
	}
	packages := map[string]*pkgState{}
	state := func(name string) *pkgState {
		if packages[name] == nil {
			packages[name] = &pkgState{
				versions: map[string]json.RawMessage{},
				tarballs: map[string]api.HostedManifest{},
				tags:     map[string]string{},
			}
		}
		return packages[name]
	}

	for _, m := range manifests {
		switch {
		case strings.HasPrefix(m.Path, "-/hosted/") && strings.Contains(m.Path, "/versions/"):
			body, err := readBlob(ctx, deps, m.SHA256)
			if err != nil {
				return err
			}
			state(m.Coord.Name).versions[m.Coord.Version] = body
		case strings.HasPrefix(m.Path, "-/hosted/") && strings.Contains(m.Path, "/dist-tags/"):
			body, err := readBlob(ctx, deps, m.SHA256)
			if err != nil {
				return err
			}
			tag := m.Path[strings.LastIndex(m.Path, "/")+1:]
			state(m.Coord.Name).tags[tag] = string(body)
		case strings.Contains(m.Path, "/-/"):
			state(m.Coord.Name).tarballs[m.Coord.Version] = m
		}
	}

	names := make([]string, 0, len(packages))
	for name := range packages {
		names = append(names, name)
	}
	sort.Strings(names)

	base := feedBase(feed)
	for _, name := range names {
		st := packages[name]
		versions := map[string]any{}
		for version, raw := range st.versions {
			tarball, ok := st.tarballs[version]
			if !ok {
				continue // unpublished: hidden from the index, blob retained
			}
			var doc map[string]any
			if err := json.Unmarshal(raw, &doc); err != nil {
				return fmt.Errorf("decode stored version %s@%s: %w", name, version, err)
			}
			short := name[strings.LastIndex(name, "/")+1:]
			doc["dist"] = map[string]any{
				"tarball":   base + "/" + tarballPath(name, short+"-"+version+".tgz"),
				"shasum":    tarball.Checksums["sha1"],
				"integrity": integrityFrom(tarball.Checksums),
			}
			versions[version] = doc
		}
		if len(versions) == 0 {
			continue
		}

		tags := map[string]string{}
		for tag, version := range st.tags {
			if _, ok := versions[version]; ok {
				tags[tag] = version
			}
		}
		if _, ok := tags["latest"]; !ok {
			tags["latest"] = highestVersion(versions)
		}

		root := map[string]any{
			"name":      name,
			"dist-tags": tags,
			"versions":  versions,
		}
		body, err := json.MarshalIndent(root, "", "  ")
		if err != nil {
			return fmt.Errorf("encode package root %s: %w", name, err)
		}
		body = append(body, '\n')
		if err := deps.PutIndex(ctx, feed, name, body); err != nil {
			return err
		}
		tagsBody, err := json.MarshalIndent(tags, "", "  ")
		if err != nil {
			return fmt.Errorf("encode dist-tags %s: %w", name, err)
		}
		if err := deps.PutIndex(ctx, feed, "-/package/"+name+"/dist-tags", append(tagsBody, '\n')); err != nil {
			return err
		}
	}
	return nil
}

func readBlob(ctx context.Context, deps api.CoreServices, sha256hex string) ([]byte, error) {
	rc, _, err := deps.Blobs().Get(ctx, "blobs/sha256/"+sha256hex)
	if err != nil {
		return nil, fmt.Errorf("read stored document %s: %w", sha256hex, err)
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(io.LimitReader(rc, 8<<20))
}

// integrityFrom renders the Subresource-Integrity string npm expects.
func integrityFrom(checksums map[string]string) string {
	raw, err := hex.DecodeString(checksums["sha512"])
	if err != nil || len(raw) == 0 {
		return ""
	}
	return "sha512-" + base64.StdEncoding.EncodeToString(raw)
}

// highestVersion picks the fallback "latest" tag deterministically.
func highestVersion(versions map[string]any) string {
	list := make([]string, 0, len(versions))
	for v := range versions {
		list = append(list, v)
	}
	sort.Strings(list)
	if len(list) == 0 {
		return ""
	}
	return list[len(list)-1]
}
