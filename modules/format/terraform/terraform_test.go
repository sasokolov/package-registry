package terraform

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sasokolov/package-registry/core/api"
)

func parse(t *testing.T, path string) (api.Intent, error) {
	t.Helper()
	return Module{}.Parse(httptest.NewRequest("GET", path, nil))
}

func TestParseVersions(t *testing.T) {
	intent, err := parse(t, "/v1/modules/testns/mymod/generic/versions")
	if err != nil {
		t.Fatal(err)
	}
	if intent.Kind != api.IntentMetadata || intent.CacheTTL != versionsTTL {
		t.Errorf("intent = %+v", intent)
	}
	if intent.Coord.Name != "testns/mymod/generic" || intent.Coord.Version != "" {
		t.Errorf("coord = %+v", intent.Coord)
	}
	if intent.RemotePath != "v1/modules/testns/mymod/generic/versions" {
		t.Errorf("remote path = %q", intent.RemotePath)
	}
	if intent.ContentType != "application/json" {
		t.Errorf("content type = %q", intent.ContentType)
	}
}

func TestParseDownloadIsSynthetic(t *testing.T) {
	intent, err := parse(t, "/v1/modules/testns/mymod/generic/2.0.0/download")
	if err != nil {
		t.Fatal(err)
	}
	if intent.Kind != api.IntentSynthetic {
		t.Fatalf("kind = %s", intent.Kind)
	}
	if intent.Coord.Version != "2.0.0" {
		t.Errorf("coord = %+v", intent.Coord)
	}

	// With site.external_url configured: absolute URL (terraform rejects
	// bare relative locations).
	resp, err := Module{}.Synthesize(api.Feed{Name: "tf", ExternalURL: "https://registry.local"}, intent)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != http.StatusNoContent {
		t.Errorf("status = %d", resp.Status)
	}
	wantGet := "https://registry.local/terraform/tf/v1/modules/testns/mymod/generic/2.0.0/archive.tar.gz"
	if resp.Header["X-Terraform-Get"] != wantGet {
		t.Errorf("X-Terraform-Get = %q, want %q", resp.Header["X-Terraform-Get"], wantGet)
	}

	// Without external_url: "./"-prefixed relative location, which
	// terraform resolves against the download URL (a bare "archive.tar.gz"
	// is rejected by the client).
	resp, err = Module{}.Synthesize(api.Feed{Name: "tf"}, intent)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header["X-Terraform-Get"] != "./archive.tar.gz" {
		t.Errorf("fallback X-Terraform-Get = %q, want ./archive.tar.gz", resp.Header["X-Terraform-Get"])
	}
}

func TestParseArchiveIsIndirectArtifact(t *testing.T) {
	intent, err := parse(t, "/v1/modules/testns/mymod/generic/2.0.0/archive.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if intent.Kind != api.IntentArtifact || !intent.Indirect {
		t.Fatalf("intent = %+v", intent)
	}
	if intent.RemotePath != "v1/modules/testns/mymod/generic/2.0.0/download" {
		t.Errorf("remote path = %q (must target the upstream download endpoint)", intent.RemotePath)
	}
	if intent.Coord.Name != "testns/mymod/generic" || intent.Coord.Version != "2.0.0" {
		t.Errorf("coord = %+v", intent.Coord)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, p := range []string{
		"/", "/v1", "/v1/modules", "/v2/modules/a/b/c/versions",
		"/v1/modules/a/b/versions", "/v1/modules/a/b/c/d/e/f",
		"/v1/modules/a/../c/d/versions", "/v1/modules/a/b/c/1.0/steal",
	} {
		if _, err := parse(t, p); !errors.Is(err, api.ErrNotFound) {
			t.Errorf("Parse(%q) = %v, want ErrNotFound", p, err)
		}
	}
}

func TestResolveIndirect(t *testing.T) {
	m := Module{}
	target, err := m.ResolveIndirect(api.Feed{}, api.Intent{}, http.StatusNoContent,
		map[string][]string{"X-Terraform-Get": {"/archives/mod-2.0.0.tar.gz"}}, nil)
	if err != nil || target.Location != "/archives/mod-2.0.0.tar.gz" {
		t.Errorf("ResolveIndirect = %+v, %v", target, err)
	}
	if !target.Checksum.IsZero() {
		t.Errorf("unexpected checksum %+v", target.Checksum)
	}

	if _, err := m.ResolveIndirect(api.Feed{}, api.Intent{}, http.StatusNoContent, map[string][]string{}, nil); err == nil {
		t.Error("missing header accepted")
	}
	// VCS-backed modules (public registry.terraform.io) are not proxyable.
	_, err = m.ResolveIndirect(api.Feed{}, api.Intent{}, http.StatusNoContent,
		map[string][]string{"X-Terraform-Get": {"git::https://github.com/x/y?ref=abc"}}, nil)
	if !errors.Is(err, api.ErrUpstreamUnavailable) {
		t.Errorf("git:: source: err = %v, want ErrUpstreamUnavailable", err)
	}
	if _, err := m.ResolveIndirect(api.Feed{}, api.Intent{}, http.StatusBadGateway,
		map[string][]string{"X-Terraform-Get": {"x"}}, nil); err == nil {
		t.Error("bad status accepted")
	}
}

func TestResolveIndirectGetterParams(t *testing.T) {
	m := Module{}
	digest := strings.Repeat("ab", 32)

	// go-getter checksum parameter becomes the expected checksum and is
	// stripped from the fetched URL (invariant 5).
	target, err := m.ResolveIndirect(api.Feed{}, api.Intent{}, http.StatusNoContent,
		map[string][]string{"X-Terraform-Get": {"https://cdn.example.com/m.tar.gz?checksum=sha256:" + digest}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if target.Location != "https://cdn.example.com/m.tar.gz" {
		t.Errorf("location = %q, checksum param must be stripped", target.Location)
	}
	if target.Checksum != (api.Checksum{Algo: "sha256", Hex: digest}) {
		t.Errorf("checksum = %+v", target.Checksum)
	}

	// Other query parameters survive.
	target, err = m.ResolveIndirect(api.Feed{}, api.Intent{}, http.StatusNoContent,
		map[string][]string{"X-Terraform-Get": {"https://cdn.example.com/m.tar.gz?token=t&checksum=sha1:" + strings.Repeat("c", 40)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(target.Location, "token=t") || strings.Contains(target.Location, "checksum") {
		t.Errorf("location = %q", target.Location)
	}

	// Unsupported forms are rejected rather than silently mis-cached.
	for _, loc := range []string{
		"https://cdn.example.com/m.tar.gz//subdir",
		"https://cdn.example.com/m.tar.gz?checksum=crc32:1234",
		"https://cdn.example.com/m.tar.gz?checksum=nonsense",
	} {
		if _, err := m.ResolveIndirect(api.Feed{}, api.Intent{}, http.StatusNoContent,
			map[string][]string{"X-Terraform-Get": {loc}}, nil); err == nil {
			t.Errorf("ResolveIndirect(%q) succeeded, want error", loc)
		}
	}
}

func TestValidateFeeds(t *testing.T) {
	m := Module{}
	if err := m.ValidateFeeds([]api.Feed{{Name: "tf"}}); err != nil {
		t.Errorf("single feed rejected: %v", err)
	}
	err := m.ValidateFeeds([]api.Feed{{Name: "tf"}, {Name: "other"}})
	if err == nil {
		t.Fatal("two terraform feeds accepted; discovery is host-wide")
	}
	if !strings.Contains(err.Error(), "one feed per site") {
		t.Errorf("error = %v", err)
	}
}

func TestServeRootDiscovery(t *testing.T) {
	rec := httptest.NewRecorder()
	Module{}.ServeRoot(rec, httptest.NewRequest("GET", "/.well-known/terraform.json", nil), []api.Feed{
		{Name: "zeta", Format: "terraform"},
		{Name: "alpha", Format: "terraform"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var doc map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["modules.v1"] != "/terraform/alpha/v1/modules/" {
		t.Errorf("modules.v1 = %q (lexicographically first feed must win)", doc["modules.v1"])
	}

	rec = httptest.NewRecorder()
	Module{}.ServeRoot(rec, httptest.NewRequest("GET", "/.well-known/terraform.json", nil), nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("no-feeds status = %d, want 404", rec.Code)
	}
}

// Golden test: the versions document passes through byte-identical.
func TestRewriteMetadataGolden(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "versions.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Module{}.RewriteMetadata(api.Feed{Name: "tf"}, raw)
	if err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile(filepath.Join("testdata", "versions.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(golden) {
		t.Errorf("RewriteMetadata output differs from golden")
	}
	if !strings.Contains(string(got), `"versions"`) {
		t.Error("golden fixture is not a versions document")
	}
}
