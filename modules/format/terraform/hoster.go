package terraform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/fondaco-dev/fondaco/core/api"
)

// maxArchiveSize bounds a module upload.
const maxArchiveSize = 512 << 20

// HandlePublish implements api.Hoster: upload a module version archive with
//
//	PUT /v1/modules/{ns}/{name}/{provider}/{version}/archive.tar.gz
//
// The archive is stored as a content-addressed blob and committed as an
// immutable coordinate; the versions document is derived data rebuilt by
// Reindex.
func (Module) HandlePublish(ctx context.Context, feed api.Feed, r *http.Request, deps api.CoreServices) error {
	p := strings.TrimPrefix(r.URL.Path, "/")
	segs := strings.Split(p, "/")
	if len(segs) != 7 || segs[0] != "v1" || segs[1] != "modules" || segs[6] != archiveFile {
		return fmt.Errorf("upload path must be /v1/modules/{ns}/{name}/{provider}/{version}/%s: %w",
			archiveFile, api.ErrBadRequest)
	}
	rest := segs[2:]
	for _, s := range rest {
		if s == "" || s == "." || s == ".." {
			return fmt.Errorf("invalid upload path %q: %w", p, api.ErrBadRequest)
		}
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxArchiveSize))
	if err != nil {
		return fmt.Errorf("read archive: %w", err)
	}
	if len(body) == 0 {
		return fmt.Errorf("empty archive: %w", api.ErrBadRequest)
	}
	if !bytes.HasPrefix(body, []byte{0x1f, 0x8b}) {
		return fmt.Errorf("archive must be gzip (tar.gz): %w", api.ErrBadRequest)
	}
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])

	if err := deps.Blobs().Put(ctx, "blobs/sha256/"+digest, bytes.NewReader(body),
		api.PutOpts{SHA256: digest, Size: int64(len(body))}); err != nil {
		return fmt.Errorf("stage archive: %w", err)
	}

	_, err = deps.Publish(ctx, api.PublishRequest{
		Feed:  feed,
		Coord: coord(rest, rest[3]),
		// Not the upload path: the path a GET for this archive resolves
		// to. They differ because Terraform downloads through an
		// indirection, and publishing under the upload path would store an
		// archive that no request can reach.
		Path:      archiveIntentPath(p),
		SHA256:    digest,
		Size:      int64(len(body)),
		Checksums: map[string]string{"sha256": digest},
		Metadata:  map[string]string{api.MetaEcosystem: "Terraform"},
	})
	return err
}

// versionsDoc is the Module Registry v1 versions response.
type versionsDoc struct {
	Modules []versionsModule `json:"modules"`
}

type versionsModule struct {
	Source   string          `json:"source"`
	Versions []versionsEntry `json:"versions"`
}

type versionsEntry struct {
	Version string `json:"version"`
}

// Reindex implements api.Hoster: rebuild each module's versions document
// from the hosted manifest set. Deterministic: same manifests, same bytes.
func (Module) Reindex(ctx context.Context, feed api.Feed, deps api.CoreServices) error {
	manifests, err := deps.Manifests(ctx, feed, "v1/modules/")
	if err != nil {
		return err
	}
	bySource := make(map[string]map[string]bool)
	for _, m := range manifests {
		if m.Coord.Version == "" || m.Coord.Name == "" {
			continue
		}
		if bySource[m.Coord.Name] == nil {
			bySource[m.Coord.Name] = make(map[string]bool)
		}
		bySource[m.Coord.Name][m.Coord.Version] = true
	}

	sources := make([]string, 0, len(bySource))
	for s := range bySource {
		sources = append(sources, s)
	}
	sort.Strings(sources)

	for _, source := range sources {
		list := make([]string, 0, len(bySource[source]))
		for v := range bySource[source] {
			list = append(list, v)
		}
		sort.Strings(list)
		entries := make([]versionsEntry, 0, len(list))
		for _, v := range list {
			entries = append(entries, versionsEntry{Version: v})
		}
		doc := versionsDoc{Modules: []versionsModule{{Source: source, Versions: entries}}}
		body, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return fmt.Errorf("encode versions document: %w", err)
		}
		body = append(body, '\n')
		if err := deps.PutIndex(ctx, feed, "v1/modules/"+source+"/versions", body); err != nil {
			return err
		}
	}
	return nil
}
