package nuget

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/sasokolov/package-registry/core/api"
)

// Hosting for NuGet.
//
// A push is one .nupkg — a zip with a .nuspec inside it — and everything the
// protocol serves afterwards is derived from that file. The nuspec is
// extracted once, at publish, and kept as its own coordinate: rebuilding the
// registration documents must not mean re-opening every package in the feed,
// and a geo peer that received only manifests has to be able to rebuild them
// too (invariant 15).
//
// Layout note: hosted content is stored under the same paths the proxy
// caches upstream content at, because a request resolves to one path
// regardless of which feed answers it. That is also what lets a group
// contain a hosted and a proxied feed and ask both the same question.

// maxPushSize bounds one uploaded package.
const maxPushSize = 512 << 20

// hostedStatePrefix holds the extracted nuspec facts. No NuGet request can
// address it: Parse only accepts v3/ and api/ paths, so this is invisible to
// clients while still being a replicated fact rather than a derived index.
const hostedStatePrefix = "-hosted/"

// publishPath is where `dotnet nuget push` sends packages, relative to the
// feed mount.
const publishPath = "/api/v2/package"

// PublishRoutes implements api.PublishRouter.
func (Module) PublishRoutes() []api.Route {
	return []api.Route{
		{Method: http.MethodPut, Pattern: publishPath},
		{Method: http.MethodPut, Pattern: publishPath + "/"},
	}
}

// CredentialHeaders implements api.CredentialHeader: `dotnet nuget push -k`
// sends the credential as an API key, not as an Authorization header.
func (Module) CredentialHeaders() []string {
	return []string{"X-NuGet-ApiKey", "X-Nuget-Apikey"}
}

// nuspec is the part of the package manifest the registry interprets.
type nuspec struct {
	XMLName  xml.Name `xml:"package"`
	Metadata struct {
		ID          string `xml:"id"`
		Version     string `xml:"version"`
		Authors     string `xml:"authors"`
		Description string `xml:"description"`
		// License is the modern form; LicenseURL is the deprecated one that
		// older packages still carry.
		License struct {
			Type  string `xml:"type,attr"`
			Value string `xml:",chardata"`
		} `xml:"license"`
		LicenseURL   string `xml:"licenseUrl"`
		ProjectURL   string `xml:"projectUrl"`
		Dependencies struct {
			// Dependencies are either grouped by target framework or listed
			// flat; both shapes appear in the wild.
			Groups []struct {
				TargetFramework string `xml:"targetFramework,attr"`
				Dependencies    []struct {
					ID      string `xml:"id,attr"`
					Version string `xml:"version,attr"`
				} `xml:"dependency"`
			} `xml:"group"`
			Flat []struct {
				ID      string `xml:"id,attr"`
				Version string `xml:"version,attr"`
			} `xml:"dependency"`
		} `xml:"dependencies"`
	} `xml:"metadata"`
}

// hostedVersion is what a publish records about one package version, and
// what Reindex rebuilds the protocol documents from. It is deliberately the
// facts, not a rendered document: the documents are derived and are rebuilt
// locally at every site.
type hostedVersion struct {
	ID          string            `json:"id"`
	Version     string            `json:"version"`
	Authors     string            `json:"authors,omitempty"`
	Description string            `json:"description,omitempty"`
	License     string            `json:"license,omitempty"`
	LicenseURL  string            `json:"license_url,omitempty"`
	ProjectURL  string            `json:"project_url,omitempty"`
	Published   string            `json:"published"`
	Groups      []dependencyGroup `json:"dependency_groups,omitempty"`
}

type dependencyGroup struct {
	TargetFramework string       `json:"target_framework,omitempty"`
	Dependencies    []dependency `json:"dependencies,omitempty"`
}

type dependency struct {
	ID    string `json:"id"`
	Range string `json:"range,omitempty"`
}

// HandlePublish implements api.Hoster: accept one pushed .nupkg.
func (Module) HandlePublish(ctx context.Context, feed api.Feed, r *http.Request, deps api.CoreServices) error {
	if feed.ExternalURL == "" {
		// Registration documents must carry absolute URLs, and there is no
		// honest way to write one without knowing this site's address.
		// Refusing here beats publishing a package no client can resolve.
		return fmt.Errorf(
			"hosted NuGet needs site.external_url: registration documents must carry absolute URLs: %w",
			api.ErrBadRequest)
	}

	raw, err := readPushedPackage(r)
	if err != nil {
		return err
	}

	spec, specBody, err := readNuspec(raw)
	if err != nil {
		return err
	}
	id, version := spec.Metadata.ID, spec.Metadata.Version
	if id == "" || version == "" {
		return fmt.Errorf("the package manifest has no id or version: %w", api.ErrBadRequest)
	}

	coord := api.PackageCoordinate{Format: "nuget", Name: id, Version: version}
	meta := map[string]string{api.MetaEcosystem: "NuGet"}
	if license := licenseOf(spec); license != "" {
		meta[api.MetaLicense] = license
	}

	// The package itself.
	if err := publishBlob(ctx, feed, deps, nupkgPath(id, version), coord, raw, meta, false); err != nil {
		return err
	}
	// The manifest, which the flat container serves beside it.
	if err := publishBlob(ctx, feed, deps, nuspecPath(id, version), coord, specBody, meta, false); err != nil {
		return err
	}
	// And the facts the registration documents are rebuilt from.
	state := hostedVersion{
		ID: id, Version: version,
		Authors:     spec.Metadata.Authors,
		Description: spec.Metadata.Description,
		License:     licenseOf(spec),
		LicenseURL:  spec.Metadata.LicenseURL,
		ProjectURL:  spec.Metadata.ProjectURL,
		Published:   publishedAt(raw),
		Groups:      dependencyGroupsOf(spec),
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode package facts: %w", err)
	}
	return publishBlob(ctx, feed, deps, statePath(id, version), coord, encoded, meta, true)
}

// readPushedPackage pulls the .nupkg out of the multipart body NuGet sends.
func readPushedPackage(r *http.Request) ([]byte, error) {
	contentType := r.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		// Some tooling PUTs the package bytes directly; accept that too
		// rather than insisting on the shape the official client happens
		// to use.
		body, err := io.ReadAll(io.LimitReader(r.Body, maxPushSize))
		if err != nil {
			return nil, fmt.Errorf("read pushed package: %w", err)
		}
		if len(body) == 0 {
			return nil, fmt.Errorf("the push carried no package: %w", api.ErrBadRequest)
		}
		return body, nil
	}
	if params["boundary"] == "" {
		return nil, fmt.Errorf("multipart push without a boundary: %w", api.ErrBadRequest)
	}

	reader, err := r.MultipartReader()
	if err != nil {
		return nil, fmt.Errorf("read multipart push: %v: %w", err, api.ErrBadRequest)
	}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return nil, fmt.Errorf("the push carried no package: %w", api.ErrBadRequest)
		}
		if err != nil {
			return nil, fmt.Errorf("read multipart push: %v: %w", err, api.ErrBadRequest)
		}
		body, err := io.ReadAll(io.LimitReader(part, maxPushSize))
		_ = part.Close()
		if err != nil {
			return nil, fmt.Errorf("read pushed package: %w", err)
		}
		if len(body) > 0 {
			return body, nil
		}
	}
}

// readNuspec finds and parses the manifest inside a .nupkg.
func readNuspec(pkg []byte) (nuspec, []byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(pkg), int64(len(pkg)))
	if err != nil {
		return nuspec{}, nil, fmt.Errorf("the pushed package is not a valid .nupkg: %v: %w", err, api.ErrBadRequest)
	}
	for _, f := range zr.File {
		// The manifest lives at the archive root; entries deeper in are
		// content files that happen to end in .nuspec.
		if strings.Contains(f.Name, "/") || !strings.HasSuffix(strings.ToLower(f.Name), ".nuspec") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nuspec{}, nil, fmt.Errorf("open %s: %w", f.Name, err)
		}
		body, err := io.ReadAll(io.LimitReader(rc, 8<<20))
		_ = rc.Close()
		if err != nil {
			return nuspec{}, nil, fmt.Errorf("read %s: %w", f.Name, err)
		}
		var spec nuspec
		if err := xml.Unmarshal(body, &spec); err != nil {
			return nuspec{}, nil, fmt.Errorf("parse %s: %v: %w", f.Name, err, api.ErrBadRequest)
		}
		return spec, body, nil
	}
	return nuspec{}, nil, fmt.Errorf("the pushed package contains no .nuspec: %w", api.ErrBadRequest)
}

// publishBlob stages content and commits one coordinate.
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
	sha256sum := sha256.Sum256(body)
	sha512sum := sha512.Sum512(body)
	return map[string]string{
		"sha256": hex.EncodeToString(sha256sum[:]),
		"sha512": hex.EncodeToString(sha512sum[:]),
	}
}

// Storage paths. Lowercase because that is how every NuGet client addresses
// the flat container and registration, and a stored path that differs from
// the requested one is a package nobody can download.
func nupkgPath(id, version string) string {
	lid, lver := strings.ToLower(id), strings.ToLower(version)
	return upstreamFlatPrefix + lid + "/" + lver + "/" + lid + "." + lver + ".nupkg"
}

func nuspecPath(id, version string) string {
	lid, lver := strings.ToLower(id), strings.ToLower(version)
	return upstreamFlatPrefix + lid + "/" + lver + "/" + lid + ".nuspec"
}

func statePath(id, version string) string {
	return hostedStatePrefix + strings.ToLower(id) + "/" + strings.ToLower(version) + ".json"
}

func flatIndexPath(id string) string {
	return upstreamFlatPrefix + strings.ToLower(id) + "/index.json"
}

func registrationIndexPath(id string) string {
	return upstreamRegistrationPrefix + strings.ToLower(id) + "/index.json"
}

// licenseOf prefers the SPDX expression and falls back to the deprecated
// licenseUrl, so a policy sees something for old packages too.
func licenseOf(spec nuspec) string {
	if value := strings.TrimSpace(spec.Metadata.License.Value); value != "" {
		return value
	}
	return strings.TrimSpace(spec.Metadata.LicenseURL)
}

// publishedAt is derived from the package, not from the clock: two sites
// rebuilding the same registration must produce the same bytes, and a wall
// clock would make that impossible (invariant 15).
func publishedAt(pkg []byte) string {
	zr, err := zip.NewReader(bytes.NewReader(pkg), int64(len(pkg)))
	if err != nil {
		return time.Time{}.UTC().Format(time.RFC3339)
	}
	newest := time.Time{}
	for _, f := range zr.File {
		if f.Modified.After(newest) {
			newest = f.Modified
		}
	}
	return newest.UTC().Format(time.RFC3339)
}

func dependencyGroupsOf(spec nuspec) []dependencyGroup {
	var groups []dependencyGroup
	for _, g := range spec.Metadata.Dependencies.Groups {
		group := dependencyGroup{TargetFramework: g.TargetFramework}
		for _, d := range g.Dependencies {
			group.Dependencies = append(group.Dependencies, dependency{ID: d.ID, Range: d.Version})
		}
		groups = append(groups, group)
	}
	if len(spec.Metadata.Dependencies.Flat) > 0 {
		group := dependencyGroup{}
		for _, d := range spec.Metadata.Dependencies.Flat {
			group.Dependencies = append(group.Dependencies, dependency{ID: d.ID, Range: d.Version})
		}
		groups = append(groups, group)
	}
	return groups
}

// Reindex implements api.Hoster: rebuild the flat-container version list and
// the registration index of every hosted package.
//
// It is a pure function of the published facts: the same manifests always
// produce the same bytes, which is what lets a geo peer replicate manifests
// only and rebuild these documents locally.
func (Module) Reindex(ctx context.Context, feed api.Feed, deps api.CoreServices) error {
	manifests, err := deps.Manifests(ctx, feed, "")
	if err != nil {
		return err
	}

	byID := map[string]map[string]hostedVersion{}
	for _, m := range manifests {
		if !strings.HasPrefix(m.Path, hostedStatePrefix) {
			continue
		}
		body, err := readBlob(ctx, deps, m.SHA256)
		if err != nil {
			return err
		}
		var state hostedVersion
		if err := json.Unmarshal(body, &state); err != nil {
			return fmt.Errorf("parse package facts at %s: %w", m.Path, err)
		}
		lid := strings.ToLower(state.ID)
		if byID[lid] == nil {
			byID[lid] = map[string]hostedVersion{}
		}
		byID[lid][state.Version] = state
	}

	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	base := feedBase(feed)
	for _, lid := range ids {
		states := byID[lid]
		versions := make([]string, 0, len(states))
		for v := range states {
			versions = append(versions, v)
		}
		sort.Slice(versions, func(i, j int) bool {
			return compareNuGetVersions(versions[i], versions[j]) < 0
		})

		flat, err := json.MarshalIndent(map[string]any{"versions": versions}, "", "  ")
		if err != nil {
			return fmt.Errorf("encode flat index for %s: %w", lid, err)
		}
		if err := deps.PutIndex(ctx, feed, flatIndexPath(lid), append(flat, '\n')); err != nil {
			return err
		}

		registration, err := buildRegistration(base, lid, versions, states)
		if err != nil {
			return err
		}
		if err := deps.PutIndex(ctx, feed, registrationIndexPath(lid), registration); err != nil {
			return err
		}
	}
	return nil
}

// buildRegistration renders the document `dotnet restore` resolves versions
// and dependencies from. One inline page: a hosted feed is not nuget.org,
// and paging a list the client will read in full only adds round trips.
func buildRegistration(base, lid string, versions []string, states map[string]hostedVersion) ([]byte, error) {
	indexURL := base + "/v3/registration/" + lid + "/index.json"
	items := make([]any, 0, len(versions))

	for _, version := range versions {
		state := states[version]
		lver := strings.ToLower(version)
		content := base + "/v3/flat2/" + lid + "/" + lver + "/" + lid + "." + lver + ".nupkg"

		catalog := map[string]any{
			"@id":            base + "/v3/registration/" + lid + "/" + lver + ".json",
			"id":             state.ID,
			"version":        state.Version,
			"listed":         true,
			"published":      state.Published,
			"packageContent": content,
		}
		if state.Authors != "" {
			catalog["authors"] = state.Authors
		}
		if state.Description != "" {
			catalog["description"] = state.Description
		}
		if state.License != "" {
			catalog["licenseExpression"] = state.License
		}
		if state.LicenseURL != "" {
			catalog["licenseUrl"] = state.LicenseURL
		}
		if state.ProjectURL != "" {
			catalog["projectUrl"] = state.ProjectURL
		}
		if groups := renderDependencyGroups(state.Groups); len(groups) > 0 {
			catalog["dependencyGroups"] = groups
		}

		items = append(items, map[string]any{
			"@id":            base + "/v3/registration/" + lid + "/" + lver + ".json",
			"packageContent": content,
			"catalogEntry":   catalog,
		})
	}

	page := map[string]any{
		"@id":   indexURL + "#page",
		"count": len(items),
		"items": items,
	}
	if len(versions) > 0 {
		page["lower"] = versions[0]
		page["upper"] = versions[len(versions)-1]
	}

	doc := map[string]any{
		"@id":   indexURL,
		"count": 1,
		"items": []any{page},
	}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode registration for %s: %w", lid, err)
	}
	return append(body, '\n'), nil
}

func renderDependencyGroups(groups []dependencyGroup) []any {
	out := make([]any, 0, len(groups))
	for _, g := range groups {
		rendered := map[string]any{}
		if g.TargetFramework != "" {
			rendered["targetFramework"] = g.TargetFramework
		}
		deps := make([]any, 0, len(g.Dependencies))
		for _, d := range g.Dependencies {
			entry := map[string]any{"id": d.ID}
			if d.Range != "" {
				entry["range"] = d.Range
			}
			deps = append(deps, entry)
		}
		if len(deps) > 0 {
			rendered["dependencies"] = deps
		}
		out = append(out, rendered)
	}
	return out
}

func readBlob(ctx context.Context, deps api.CoreServices, sha string) ([]byte, error) {
	rc, _, err := deps.Blobs().Get(ctx, "blobs/sha256/"+sha)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(io.LimitReader(rc, 8<<20))
}
