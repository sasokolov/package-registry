package composer

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // Composer's dist.shasum is sha1 by definition
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/sasokolov/package-registry/core/api"
)

// Hosting for Composer.
//
// Composer has no publish command. Packagist reads packages out of version
// control and Satis renders a static repository from the same; there is no
// wire protocol for "here is a package, store it". So this defines one, and
// picks the least surprising shape available: you PUT an archive exactly
// where it will be served from.
//
//	PUT /packages/{vendor}/{name}/{version}.zip
//	GET /packages/{vendor}/{name}/{version}.zip
//
// The archive is an ordinary Composer dist — the same zip a `composer
// archive` or a CI job produces — and its composer.json is the manifest.
// Everything Composer then reads (p2 documents, the root manifest) is
// derived from it.

// maxUploadSize bounds one uploaded archive.
const maxUploadSize = 512 << 20

// hostedDistPrefix is where hosted archives live, in the protocol path so a
// generated document can point straight at them. It cannot collide with the
// upstream dist prefix ("dists/") or with "packages.json", which Parse
// matches exactly.
const hostedDistPrefix = "packages/"

// hostedStatePrefix holds the facts read out of each archive. No Composer
// request reaches it: Parse accepts only packages.json, p2/, dists/ and
// packages/.
const hostedStatePrefix = "-hosted/"

// PublishRoutes implements api.PublishRouter.
func (Module) PublishRoutes() []api.Route {
	return []api.Route{{Method: http.MethodPut, Pattern: "/" + hostedDistPrefix + "*"}}
}

// composerManifest is the part of a package's composer.json the registry
// interprets. Everything else is passed through untouched, because Composer
// reads far more of it than a registry has any business understanding.
type composerManifest struct {
	Name        string          `json:"name"`
	Type        string          `json:"type"`
	Description string          `json:"description"`
	License     json.RawMessage `json:"license"`
	Require     json.RawMessage `json:"require"`
	RequireDev  json.RawMessage `json:"require-dev"`
	Autoload    json.RawMessage `json:"autoload"`
	Extra       json.RawMessage `json:"extra"`
}

// hostedRelease is what a publish records about one version, and what
// Reindex rebuilds the p2 documents from.
type hostedRelease struct {
	Name        string          `json:"name"`
	Version     string          `json:"version"`
	Type        string          `json:"type,omitempty"`
	Description string          `json:"description,omitempty"`
	License     json.RawMessage `json:"license,omitempty"`
	Require     json.RawMessage `json:"require,omitempty"`
	RequireDev  json.RawMessage `json:"require_dev,omitempty"`
	Autoload    json.RawMessage `json:"autoload,omitempty"`
	SHA1        string          `json:"sha1"`
	Path        string          `json:"path"`
}

// HandlePublish implements api.Hoster.
func (Module) HandlePublish(ctx context.Context, feed api.Feed, r *http.Request, deps api.CoreServices) error {
	vendor, name, version, err := parseUploadPath(strings.TrimPrefix(r.URL.Path, "/"))
	if err != nil {
		return err
	}

	archive, err := io.ReadAll(io.LimitReader(r.Body, maxUploadSize))
	if err != nil {
		return fmt.Errorf("read uploaded archive: %w", err)
	}
	if len(archive) == 0 {
		return fmt.Errorf("the upload carried no archive: %w", api.ErrBadRequest)
	}

	manifest, err := readComposerManifest(archive)
	if err != nil {
		return err
	}
	full := vendor + "/" + name
	if manifest.Name != "" && manifest.Name != full {
		// The path and the archive disagree about what this package is.
		// Guessing which one is right is how a package ends up published
		// under a name nobody expected.
		return fmt.Errorf("the archive declares %q but was uploaded as %q: %w",
			manifest.Name, full, api.ErrBadRequest)
	}

	coord := api.PackageCoordinate{Format: "composer", Name: full, Version: version}
	meta := map[string]string{api.MetaEcosystem: "Packagist"}
	if license := firstLicense(manifest.License); license != "" {
		meta[api.MetaLicense] = license
	}

	path := hostedDistPath(vendor, name, version)
	sha1sum := sha1.Sum(archive) //nolint:gosec // Composer's dist.shasum is sha1
	if err := publishBlob(ctx, feed, deps, path, coord, archive, meta, false); err != nil {
		return err
	}

	state := hostedRelease{
		Name: full, Version: version,
		Type: manifest.Type, Description: manifest.Description,
		License: manifest.License, Require: manifest.Require,
		RequireDev: manifest.RequireDev, Autoload: manifest.Autoload,
		SHA1: hex.EncodeToString(sha1sum[:]), Path: path,
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode package facts: %w", err)
	}
	return publishBlob(ctx, feed, deps, statePath(vendor, name, version), coord, encoded, meta, true)
}

// parseUploadPath splits packages/{vendor}/{name}/{version}.zip.
func parseUploadPath(p string) (vendor, name, version string, err error) {
	rest := strings.TrimPrefix(p, hostedDistPrefix)
	if rest == p {
		return "", "", "", fmt.Errorf(
			"upload path must be /%s{vendor}/{package}/{version}.zip: %w", hostedDistPrefix, api.ErrBadRequest)
	}
	segs := strings.Split(rest, "/")
	if len(segs) != 3 || !strings.HasSuffix(segs[2], ".zip") {
		return "", "", "", fmt.Errorf(
			"upload path must be /%s{vendor}/{package}/{version}.zip, got %q: %w",
			hostedDistPrefix, p, api.ErrBadRequest)
	}
	vendor, name = segs[0], segs[1]
	version = strings.TrimSuffix(segs[2], ".zip")
	for _, s := range []string{vendor, name, version} {
		if s == "" || s == "." || s == ".." || strings.ContainsAny(s, `\:*?"<>|`) {
			return "", "", "", fmt.Errorf("invalid upload path %q: %w", p, api.ErrBadRequest)
		}
	}
	return vendor, name, version, nil
}

// readComposerManifest finds composer.json inside a dist archive. Composer
// archives usually wrap everything in one directory, so the manifest is
// either at the root or exactly one level down; anything deeper belongs to a
// vendored dependency and is not this package's manifest.
func readComposerManifest(archive []byte) (composerManifest, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return composerManifest{}, fmt.Errorf(
			"the upload is not a valid zip archive: %v: %w", err, api.ErrBadRequest)
	}
	best := ""
	for _, f := range zr.File {
		if !strings.HasSuffix(f.Name, "composer.json") {
			continue
		}
		depth := strings.Count(f.Name, "/")
		if depth > 1 {
			continue
		}
		if best == "" || strings.Count(best, "/") > depth {
			best = f.Name
		}
	}
	if best == "" {
		return composerManifest{}, fmt.Errorf(
			"the archive contains no composer.json: %w", api.ErrBadRequest)
	}
	for _, f := range zr.File {
		if f.Name != best {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return composerManifest{}, fmt.Errorf("open %s: %w", f.Name, err)
		}
		body, err := io.ReadAll(io.LimitReader(rc, 8<<20))
		_ = rc.Close()
		if err != nil {
			return composerManifest{}, fmt.Errorf("read %s: %w", f.Name, err)
		}
		var manifest composerManifest
		if err := json.Unmarshal(body, &manifest); err != nil {
			return composerManifest{}, fmt.Errorf(
				"parse %s: %v: %w", f.Name, err, api.ErrBadRequest)
		}
		return manifest, nil
	}
	return composerManifest{}, fmt.Errorf("the archive contains no composer.json: %w", api.ErrBadRequest)
}

func publishBlob(ctx context.Context, feed api.Feed, deps api.CoreServices,
	path string, coord api.PackageCoordinate, body []byte, meta map[string]string, mutable bool) error {
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
		Mutable:   mutable,
		Metadata:  meta,
	})
	return err
}

func digestsOf(body []byte) map[string]string {
	sha1sum := sha1.Sum(body) //nolint:gosec // Composer's dist.shasum is sha1
	sha256sum := sha256.Sum256(body)
	return map[string]string{
		"sha1":   hex.EncodeToString(sha1sum[:]),
		"sha256": hex.EncodeToString(sha256sum[:]),
	}
}

func hostedDistPath(vendor, name, version string) string {
	return hostedDistPrefix + vendor + "/" + name + "/" + version + ".zip"
}

func statePath(vendor, name, version string) string {
	return hostedStatePrefix + vendor + "/" + name + "/" + version + ".json"
}

// firstLicense reduces composer.json's license field — a string or a list —
// to one value for policies.
func firstLicense(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return single
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil && len(list) > 0 {
		return list[0]
	}
	return ""
}

// Reindex implements api.Hoster: rebuild each package's p2 document and the
// root manifest from the published facts.
//
// A pure function of those facts, so a geo peer that replicated only
// manifests rebuilds byte-identical documents (invariant 15).
func (Module) Reindex(ctx context.Context, feed api.Feed, deps api.CoreServices) error {
	manifests, err := deps.Manifests(ctx, feed, hostedStatePrefix)
	if err != nil {
		return err
	}

	byPackage := map[string][]hostedRelease{}
	for _, m := range manifests {
		body, err := readBlob(ctx, deps, m.SHA256)
		if err != nil {
			return err
		}
		var state hostedRelease
		if err := json.Unmarshal(body, &state); err != nil {
			return fmt.Errorf("parse package facts at %s: %w", m.Path, err)
		}
		byPackage[state.Name] = append(byPackage[state.Name], state)
	}

	base := feedBase(feed)
	names := make([]string, 0, len(byPackage))
	for name := range byPackage {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		releases := byPackage[name]
		sort.Slice(releases, func(i, j int) bool {
			return compareComposerVersions(releases[i].Version, releases[j].Version) < 0
		})

		entries := make([]any, 0, len(releases))
		for _, release := range releases {
			entry := map[string]any{
				"name":               release.Name,
				"version":            release.Version,
				"version_normalized": normalizeVersion(release.Version),
				"dist": map[string]any{
					"type":      "zip",
					"url":       base + "/" + release.Path,
					"reference": release.SHA1,
					"shasum":    release.SHA1,
				},
			}
			if release.Type != "" {
				entry["type"] = release.Type
			} else {
				entry["type"] = "library"
			}
			if release.Description != "" {
				entry["description"] = release.Description
			}
			if len(release.License) > 0 {
				entry["license"] = release.License
			}
			if len(release.Require) > 0 {
				entry["require"] = release.Require
			}
			if len(release.RequireDev) > 0 {
				entry["require-dev"] = release.RequireDev
			}
			if len(release.Autoload) > 0 {
				entry["autoload"] = release.Autoload
			}
			entries = append(entries, entry)
		}

		doc, err := json.MarshalIndent(map[string]any{
			"packages": map[string]any{name: entries},
		}, "", "  ")
		if err != nil {
			return fmt.Errorf("encode p2 document for %s: %w", name, err)
		}
		if err := deps.PutIndex(ctx, feed, "p2/"+name+".json", append(doc, '\n')); err != nil {
			return err
		}
	}

	// The root manifest tells Composer where to look up each package. It is
	// generated even with no packages: a repository that answers 404 here
	// looks broken, and an empty one is a truthful answer.
	root, err := json.MarshalIndent(map[string]any{
		"metadata-url":       base + "/p2/%package%.json",
		"search":             base + "/" + searchPath + "?q=%query%&type=%type%",
		"available-packages": names,
		"packages":           map[string]any{},
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode root manifest: %w", err)
	}
	return deps.PutIndex(ctx, feed, "packages.json", append(root, '\n'))
}

// normalizeVersion renders Composer's four-segment normalized form.
// Composer computes it itself when absent, but every real repository emits
// it and a client that trusts it should not have to guess.
func normalizeVersion(version string) string {
	core, suffix, _ := strings.Cut(strings.TrimPrefix(version, "v"), "-")
	segs := strings.Split(core, ".")
	for len(segs) < 4 {
		segs = append(segs, "0")
	}
	for i, s := range segs {
		if n, err := strconv.Atoi(s); err == nil {
			segs[i] = strconv.Itoa(n)
		}
	}
	normalized := strings.Join(segs[:4], ".")
	if suffix != "" {
		normalized += "-" + strings.ToLower(suffix)
	}
	return normalized
}

// compareComposerVersions orders two versions: numeric segments as numbers,
// a pre-release losing to its release.
func compareComposerVersions(a, b string) int {
	aCore, aPre, _ := strings.Cut(strings.TrimPrefix(a, "v"), "-")
	bCore, bPre, _ := strings.Cut(strings.TrimPrefix(b, "v"), "-")

	aSegs, bSegs := strings.Split(aCore, "."), strings.Split(bCore, ".")
	for i := 0; i < len(aSegs) || i < len(bSegs); i++ {
		if c := compareSegment(segmentAt(aSegs, i), segmentAt(bSegs, i)); c != 0 {
			return c
		}
	}
	switch {
	case aPre == "" && bPre == "":
		return 0
	case aPre == "":
		return 1
	case bPre == "":
		return -1
	}
	return strings.Compare(aPre, bPre)
}

func segmentAt(segs []string, i int) string {
	if i < len(segs) {
		return segs[i]
	}
	return "0"
}

func compareSegment(a, b string) int {
	an, aErr := strconv.Atoi(a)
	bn, bErr := strconv.Atoi(b)
	if aErr == nil && bErr == nil {
		switch {
		case an < bn:
			return -1
		case an > bn:
			return 1
		default:
			return 0
		}
	}
	return strings.Compare(a, b)
}

func readBlob(ctx context.Context, deps api.CoreServices, sha string) ([]byte, error) {
	rc, _, err := deps.Blobs().Get(ctx, "blobs/sha256/"+sha)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(io.LimitReader(rc, 8<<20))
}
