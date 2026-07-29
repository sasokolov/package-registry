package helm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sasokolov/package-registry/core/api"
	"github.com/sasokolov/package-registry/modules/internal/semver"
)

// maxChartSize bounds an upload. Charts are templates and values, measured
// in kilobytes; a hundred megabytes is not a chart, it is a mistake or an
// attack, and finding out after buffering a gigabyte is too late.
const maxChartSize = 100 << 20

// hostedPrefix is where a published chart's own metadata is kept.
//
// The index needs a chart's description, appVersion, dependencies and the
// rest, and reading them back out of every archive on every rebuild would
// mean gunzipping the whole repository to answer one document. They are
// stored once, next to the archive, and Reindex reads them.
const hostedPrefix = "-/hosted/"

// PublishRoutes implements api.PublishRouter.
//
// ChartMuseum's upload is POST /api/charts, and its delete is
// DELETE /api/charts/{name}/{version}. The delete route is claimed here so
// that it can be refused with an explanation rather than falling through to
// "no such path", which would read as a bug in the client.
func (Module) PublishRoutes() []api.Route {
	return []api.Route{
		{Method: http.MethodPost, Pattern: "/api/charts"},
		{Method: http.MethodPost, Pattern: "/api/charts/"},
		{Method: http.MethodDelete, Pattern: "/api/charts/*"},
	}
}

// HandlePublish implements api.Hoster.
func (Module) HandlePublish(ctx context.Context, feed api.Feed, r *http.Request, deps api.CoreServices) error {
	if r.Method == http.MethodDelete {
		return fmt.Errorf(
			"%w: a published chart version is not removed, it is quarantined — the bytes "+
				"somebody deployed do not quietly stop existing (invariant 4). Use the "+
				"quarantine API to take it out of circulation",
			api.ErrForbidden)
	}
	if p := strings.Trim(strings.TrimPrefix(r.URL.Path, "/"), "/"); p != strings.TrimSuffix(apiPrefix, "/") {
		return fmt.Errorf("%w: charts are uploaded to POST /api/charts, not %q", api.ErrBadRequest, p)
	}

	chart, prov, err := readUpload(r)
	if err != nil {
		return err
	}

	meta, err := chartMetadata(chart)
	if err != nil {
		return err
	}
	if meta.Name == "" || meta.Version == "" {
		return fmt.Errorf("%w: Chart.yaml has no name or version", api.ErrBadRequest)
	}

	digest, err := stage(ctx, deps, chart)
	if err != nil {
		return err
	}
	file := chartFile(meta.Name, meta.Version)
	coord := api.PackageCoordinate{Format: "helm", Name: meta.Name, Version: meta.Version}

	if _, err := deps.Publish(ctx, api.PublishRequest{
		Feed:   feed,
		Coord:  coord,
		Path:   chartsPrefix + file,
		SHA256: digest,
		Size:   int64(len(chart)),
		Metadata: map[string]string{
			api.MetaEcosystem: "helm",
		},
	}); err != nil {
		return err
	}

	// The chart's own metadata, kept for the index. Mutable because it is a
	// projection of the archive rather than a release of its own: the
	// archive is what immutability protects.
	if err := publishMutable(ctx, feed, deps, hostedPrefix+meta.Name+"/"+meta.Version+".yaml",
		coord, meta.Raw); err != nil {
		return err
	}

	if len(prov) > 0 {
		provDigest, err := stage(ctx, deps, prov)
		if err != nil {
			return err
		}
		if _, err := deps.Publish(ctx, api.PublishRequest{
			Feed:   feed,
			Coord:  coord,
			Path:   chartsPrefix + file + ".prov",
			SHA256: provDigest,
			Size:   int64(len(prov)),
		}); err != nil {
			return err
		}
	}

	return Module{}.Reindex(ctx, feed, deps)
}

// readUpload takes the archive out of the request, in either of the two
// shapes ChartMuseum accepts: a multipart form with a "chart" field, or the
// archive as the whole body.
func readUpload(r *http.Request) (chart, prov []byte, err error) {
	contentType := r.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "multipart/form-data") {
		if err := r.ParseMultipartForm(maxChartSize); err != nil {
			return nil, nil, fmt.Errorf("%w: reading the upload: %v", api.ErrBadRequest, err)
		}
		chart, err = formFile(r, "chart")
		if err != nil {
			return nil, nil, err
		}
		prov, _ = formFile(r, "prov")
		return chart, prov, nil
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxChartSize+1))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: reading the upload: %v", api.ErrBadRequest, err)
	}
	if int64(len(body)) > maxChartSize {
		return nil, nil, fmt.Errorf("%w: chart exceeds %d bytes", api.ErrBadRequest, maxChartSize)
	}
	if len(body) == 0 {
		return nil, nil, fmt.Errorf("%w: empty upload", api.ErrBadRequest)
	}
	return body, nil, nil
}

func formFile(r *http.Request, field string) ([]byte, error) {
	file, _, err := r.FormFile(field)
	if err != nil {
		return nil, fmt.Errorf("%w: no %q in the upload", api.ErrBadRequest, field)
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, maxChartSize+1))
	if err != nil {
		return nil, fmt.Errorf("%w: reading %q: %v", api.ErrBadRequest, field, err)
	}
	if int64(len(body)) > maxChartSize {
		return nil, fmt.Errorf("%w: %s exceeds %d bytes", api.ErrBadRequest, field, maxChartSize)
	}
	return body, nil
}

// chartMeta is what Chart.yaml said, kept whole.
type chartMeta struct {
	Name    string
	Version string
	// Raw is the Chart.yaml bytes, so the index can carry every field the
	// chart declared rather than the handful this module knows about.
	Raw []byte
}

// chartMetadata finds and reads Chart.yaml inside the archive.
//
// This is the authoritative answer to what the chart is called and which
// version it is — the file name only suggests it, and both halves may
// contain a dash.
func chartMetadata(archive []byte) (chartMeta, error) {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return chartMeta{}, fmt.Errorf("%w: not a gzip archive: %v", api.ErrBadRequest, err)
	}
	defer func() { _ = zr.Close() }()

	tr := tar.NewReader(zr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return chartMeta{}, fmt.Errorf("%w: not a tar archive: %v", api.ErrBadRequest, err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		// Chart.yaml sits at the root of the chart's own directory, and
		// only there: a Chart.yaml under charts/ belongs to a subchart.
		clean := path.Clean(header.Name)
		parts := strings.Split(clean, "/")
		if len(parts) != 2 || parts[1] != "Chart.yaml" {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(tr, 1<<20))
		if err != nil {
			return chartMeta{}, fmt.Errorf("%w: reading Chart.yaml: %v", api.ErrBadRequest, err)
		}
		var doc struct {
			Name    string `yaml:"name"`
			Version string `yaml:"version"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return chartMeta{}, fmt.Errorf("%w: Chart.yaml is not YAML: %v", api.ErrBadRequest, err)
		}
		return chartMeta{Name: doc.Name, Version: doc.Version, Raw: raw}, nil
	}
	return chartMeta{}, fmt.Errorf("%w: the archive has no Chart.yaml", api.ErrBadRequest)
}

func stage(ctx context.Context, deps api.CoreServices, body []byte) (string, error) {
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	if err := deps.Blobs().Put(ctx, "blobs/sha256/"+digest, bytes.NewReader(body),
		api.PutOpts{SHA256: digest, Size: int64(len(body))}); err != nil {
		return "", fmt.Errorf("stage chart: %w", err)
	}
	return digest, nil
}

func publishMutable(ctx context.Context, feed api.Feed, deps api.CoreServices,
	path string, coord api.PackageCoordinate, body []byte) error {
	digest, err := stage(ctx, deps, body)
	if err != nil {
		return err
	}
	_, err = deps.Publish(ctx, api.PublishRequest{
		Feed: feed, Coord: coord, Path: path,
		SHA256: digest, Size: int64(len(body)), Mutable: true,
	})
	return err
}

// storedChart is one published version as the index builder sees it.
type storedChart struct {
	Name     string
	Version  string
	Digest   string
	Created  string
	Metadata map[string]any
}

// Reindex implements api.Hoster: rebuild index.yaml from what is published.
//
// It is a pure function of the manifest set — no state of its own, nothing
// carried over from the last run — which is what lets every site rebuild it
// locally instead of replicating it (invariant 15), and what makes two sites
// that hold the same charts produce the same index.
func (Module) Reindex(ctx context.Context, feed api.Feed, deps api.CoreServices) error {
	manifests, err := deps.Manifests(ctx, feed, "")
	if err != nil {
		return fmt.Errorf("list helm manifests: %w", err)
	}

	archives := map[string]api.HostedManifest{} // name@version -> archive
	metadata := map[string][]byte{}             // name@version -> Chart.yaml
	for _, m := range manifests {
		switch {
		case strings.HasPrefix(m.Path, hostedPrefix):
			body, err := readBlob(ctx, deps, m.SHA256)
			if err != nil {
				return err
			}
			metadata[m.Coord.Name+"@"+m.Coord.Version] = body
		case strings.HasPrefix(m.Path, chartsPrefix) && !strings.HasSuffix(m.Path, ".prov"):
			archives[m.Coord.Name+"@"+m.Coord.Version] = m
		}
	}

	charts := make([]storedChart, 0, len(archives))
	for key, archive := range archives {
		chart := storedChart{
			Name:    archive.Coord.Name,
			Version: archive.Coord.Version,
			Digest:  archive.SHA256,
			Created: archive.PublishedAt.UTC().Format(time.RFC3339),
		}
		if raw, ok := metadata[key]; ok {
			var doc map[string]any
			if err := yaml.Unmarshal(raw, &doc); err != nil {
				return fmt.Errorf("decode stored Chart.yaml for %s: %w", key, err)
			}
			chart.Metadata = doc
		}
		charts = append(charts, chart)
	}
	// A stable order in, a stable index out.
	sort.Slice(charts, func(i, j int) bool {
		if charts[i].Name != charts[j].Name {
			return charts[i].Name < charts[j].Name
		}
		return semver.Compare(charts[i].Version, charts[j].Version) > 0
	})

	body, err := buildIndex(feed, charts, time.Now())
	if err != nil {
		return err
	}
	return deps.PutIndex(ctx, feed, indexPath, body)
}

func readBlob(ctx context.Context, deps api.CoreServices, digest string) ([]byte, error) {
	rc, _, err := deps.Blobs().Get(ctx, "blobs/sha256/"+digest)
	if err != nil {
		return nil, fmt.Errorf("read blob %s: %w", digest, err)
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(io.LimitReader(rc, 1<<20))
}
