package pipeline

import (
	"context"
	"crypto/sha1" //nolint:gosec // legacy checksum in tests
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sasokolov/package-registry/core/api"
)

func sha1hex(s string) string {
	sum := sha1.Sum([]byte(s)) //nolint:gosec // legacy checksum in tests
	return hex.EncodeToString(sum[:])
}

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestRemoteChecksumVerifiesIngest(t *testing.T) {
	h := newHarness(t)
	h.set("lib/a.jar", "jar content")
	h.set("lib/a.jar.sha1", sha1hex("jar content")+"  a.jar\n") // "<hex>  <name>" form
	up := h.upstream(0)

	intent := artifactIntent("lib/a.jar")
	intent.RemoteChecksum = api.ChecksumSource{Algo: "sha1", Path: "lib/a.jar.sha1"}

	res, err := h.pipe.Serve(t.Context(), h.request(intent, up))
	if err != nil {
		t.Fatalf("Serve with matching remote checksum: %v", err)
	}
	if got := mustBody(t, res); got != "jar content" {
		t.Errorf("body = %q", got)
	}
}

func TestRemoteChecksumMismatchRejects(t *testing.T) {
	h := newHarness(t)
	h.set("lib/bad.jar", "tampered")
	h.set("lib/bad.jar.sha1", sha1hex("the original content"))
	up := h.upstream(0)

	intent := artifactIntent("lib/bad.jar")
	intent.RemoteChecksum = api.ChecksumSource{Algo: "sha1", Path: "lib/bad.jar.sha1"}

	if _, err := h.pipe.Serve(t.Context(), h.request(intent, up)); !errors.Is(err, api.ErrChecksumMismatch) {
		t.Fatalf("Serve = %v, want ErrChecksumMismatch", err)
	}
	if h.store.has("manifests/test-feed/lib/bad.jar") {
		t.Error("manifest stored despite remote checksum mismatch")
	}
}

func TestRemoteChecksumAbsentUpstreamIngestsUnverified(t *testing.T) {
	h := newHarness(t)
	h.set("lib/nochk.jar", "content without checksum")
	// no .sha1 upstream -> 404 -> unverified ingest
	up := h.upstream(0)

	intent := artifactIntent("lib/nochk.jar")
	intent.RemoteChecksum = api.ChecksumSource{Algo: "sha1", Path: "lib/nochk.jar.sha1"}

	res, err := h.pipe.Serve(t.Context(), h.request(intent, up))
	if err != nil {
		t.Fatalf("Serve without upstream checksum: %v", err)
	}
	_ = mustBody(t, res)
}

func TestRemoteChecksumUpstreamFailureAbortsIngest(t *testing.T) {
	h := newHarness(t)
	h.set("lib/x.jar", "content")
	h.custom["lib/x.jar.sha1"] = func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}
	up := h.upstream(0)

	intent := artifactIntent("lib/x.jar")
	intent.RemoteChecksum = api.ChecksumSource{Algo: "sha1", Path: "lib/x.jar.sha1"}

	// A flaky upstream must not silently disable verification.
	if _, err := h.pipe.Serve(t.Context(), h.request(intent, up)); err == nil {
		t.Fatal("Serve succeeded although the checksum document was unavailable")
	}
	if h.store.has("manifests/test-feed/lib/x.jar") {
		t.Error("manifest stored despite unavailable checksum document")
	}
}

func TestWantChecksumServedFromManifest(t *testing.T) {
	h := newHarness(t)
	h.set("lib/c.jar", "checksummed content")
	up := h.upstream(0)
	ctx := t.Context()

	// Checksum request BEFORE the artifact was ever fetched: it must
	// trigger the ingest itself (clients fetch .sha1 in any order).
	intent := artifactIntent("lib/c.jar")
	intent.WantChecksum = "sha1"
	res, err := h.pipe.Serve(ctx, h.request(intent, up))
	if err != nil {
		t.Fatalf("Serve checksum-first: %v", err)
	}
	if got := mustBody(t, res); got != sha1hex("checksummed content") {
		t.Errorf("sha1 body = %q, want %q", got, sha1hex("checksummed content"))
	}
	if res.Source != api.SourceUpstream {
		t.Errorf("checksum-first source = %s, want upstream", res.Source)
	}
	fetched := h.requests.Load()

	// All other algorithms come from the stored manifest: zero upstream
	// traffic (never proxied as separate requests).
	for algo, want := range map[string]string{
		"sha256": sha256hex("checksummed content"),
		"sha1":   sha1hex("checksummed content"),
	} {
		in := artifactIntent("lib/c.jar")
		in.WantChecksum = algo
		res, err := h.pipe.Serve(ctx, h.request(in, up))
		if err != nil {
			t.Fatalf("Serve %s: %v", algo, err)
		}
		if got := mustBody(t, res); got != want {
			t.Errorf("%s = %q, want %q", algo, got, want)
		}
		if res.Source != api.SourceCache {
			t.Errorf("%s source = %s, want cache", algo, res.Source)
		}
	}
	if h.requests.Load() != fetched {
		t.Errorf("checksum requests hit upstream %d extra times", h.requests.Load()-fetched)
	}
}

func TestMetadataWantChecksumHashesServedBytes(t *testing.T) {
	h := newHarness(t)
	h.set("meta/m.xml", "<metadata/>")
	up := h.upstream(0)
	ctx := t.Context()

	// The module rewrites metadata; the sidecar checksum must match the
	// bytes WE serve, not the upstream original.
	req := h.request(metadataIntent("meta/m.xml"), up)
	req.Module = echoModule{rewriteSuffix: "<!--rw-->"}
	if _, err := h.pipe.Serve(ctx, req); err != nil {
		t.Fatal(err)
	}

	chk := metadataIntent("meta/m.xml")
	chk.WantChecksum = "sha1"
	reqChk := h.request(chk, up)
	reqChk.Module = echoModule{rewriteSuffix: "<!--rw-->"}
	res, err := h.pipe.Serve(ctx, reqChk)
	if err != nil {
		t.Fatal(err)
	}
	if got := mustBody(t, res); got != sha1hex("<metadata/><!--rw-->") {
		t.Errorf("metadata sha1 = %q, want digest of the rewritten body", got)
	}
}

// indirectModule resolves the artifact location from a test header, like
// Terraform's X-Terraform-Get.
type indirectModule struct{ echoModule }

func (indirectModule) ResolveIndirect(_ api.Feed, _ api.Intent, status int, header map[string][]string, _ []byte) (api.IndirectTarget, error) {
	if status != http.StatusNoContent {
		return api.IndirectTarget{}, api.NotFoundf("unexpected indirection status %d", status)
	}
	locs := header["X-Test-Location"]
	if len(locs) == 0 {
		return api.IndirectTarget{}, errors.New("no X-Test-Location header")
	}
	target := api.IndirectTarget{Location: locs[0]}
	if sums := header["X-Test-Checksum"]; len(sums) > 0 {
		target.Checksum = api.Checksum{Algo: "sha256", Hex: sums[0]}
	}
	return target, nil
}

func TestIndirectArtifact(t *testing.T) {
	h := newHarness(t)
	h.set("real/archive.tgz", "archive bytes")
	h.custom["mod/1.0/download"] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Test-Location", "../../real/archive.tgz")
		w.WriteHeader(http.StatusNoContent)
	}
	up := h.upstream(0)
	ctx := t.Context()

	intent := artifactIntent("mod/1.0/download")
	intent.Indirect = true
	req := h.request(intent, up)
	req.Module = indirectModule{}

	res, err := h.pipe.Serve(ctx, req)
	if err != nil {
		t.Fatalf("Serve indirect: %v", err)
	}
	if got := mustBody(t, res); got != "archive bytes" {
		t.Errorf("body = %q", got)
	}
	if res.Source != api.SourceUpstream {
		t.Errorf("source = %s", res.Source)
	}

	// Second request: cached manifest, no upstream traffic at all.
	before := h.requests.Load()
	res, err = h.pipe.Serve(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != api.SourceCache {
		t.Errorf("second source = %s, want cache", res.Source)
	}
	_ = mustBody(t, res)
	if h.requests.Load() != before {
		t.Error("cached indirect artifact still touched the upstream")
	}
}

func TestIndirectChecksumVerified(t *testing.T) {
	h := newHarness(t)
	h.set("real/good.tgz", "verified archive")
	h.set("real/bad.tgz", "tampered archive")
	h.custom["mod/ok/download"] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Test-Location", "../../real/good.tgz")
		w.Header().Set("X-Test-Checksum", sha256hex("verified archive"))
		w.WriteHeader(http.StatusNoContent)
	}
	h.custom["mod/bad/download"] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Test-Location", "../../real/bad.tgz")
		w.Header().Set("X-Test-Checksum", sha256hex("what it should have been"))
		w.WriteHeader(http.StatusNoContent)
	}
	up := h.upstream(0)
	ctx := t.Context()

	good := artifactIntent("mod/ok/download")
	good.Indirect = true
	req := h.request(good, up)
	req.Module = indirectModule{}
	res, err := h.pipe.Serve(ctx, req)
	if err != nil {
		t.Fatalf("Serve with matching indirect checksum: %v", err)
	}
	_ = mustBody(t, res)

	bad := artifactIntent("mod/bad/download")
	bad.Indirect = true
	reqBad := h.request(bad, up)
	reqBad.Module = indirectModule{}
	if _, err := h.pipe.Serve(ctx, reqBad); !errors.Is(err, api.ErrChecksumMismatch) {
		t.Fatalf("Serve = %v, want ErrChecksumMismatch", err)
	}
	if h.store.has("manifests/test-feed/mod/bad/download") {
		t.Error("manifest stored despite indirect checksum mismatch")
	}
}

func TestIndirectSSRFGuard(t *testing.T) {
	h := newHarness(t)
	// The upstream points the registry at a loopback address on ANOTHER
	// host name: an upstream-controlled location must not reach internal
	// services (the feed's own upstream host stays allowed).
	h.custom["mod/evil/download"] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Test-Location", "http://localhost:9/secret")
		w.WriteHeader(http.StatusNoContent)
	}
	up := h.upstream(0)

	intent := artifactIntent("mod/evil/download")
	intent.Indirect = true
	req := h.request(intent, up)
	req.Module = indirectModule{}

	_, err := h.pipe.Serve(t.Context(), req)
	if err == nil {
		t.Fatal("Serve followed an upstream-supplied loopback location")
	}
	if !strings.Contains(err.Error(), "non-public address") {
		t.Errorf("error = %v, want a non-public address rejection", err)
	}
}

func TestRedactURL(t *testing.T) {
	tests := map[string]string{
		"https://h/p?X-Amz-Signature=secret": "https://h/p?REDACTED",
		"https://user:pw@h/p":                "https://h/p",
		"https://h/p":                        "https://h/p",
	}
	for in, want := range tests {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseChecksumBody(t *testing.T) {
	valid := strings.Repeat("ab", 20)
	tests := []struct {
		algo, body string
		want       string
		ok         bool
	}{
		{"sha1", valid, valid, true},
		{"sha1", valid + "  file.jar\n", valid, true},
		{"sha1", strings.ToUpper(valid), valid, true},
		{"sha1", "zz" + valid[2:], "", false}, // not hex
		{"sha1", "abcd", "", false},           // wrong length
		{"sha1", "", "", false},
		{"sha3", valid, "", false}, // unsupported algo
	}
	for _, tt := range tests {
		got, err := parseChecksumBody(tt.algo, []byte(tt.body))
		if tt.ok && (err != nil || got != tt.want) {
			t.Errorf("parseChecksumBody(%s, %q) = %q, %v", tt.algo, tt.body, got, err)
		}
		if !tt.ok && err == nil {
			t.Errorf("parseChecksumBody(%s, %q) succeeded, want error", tt.algo, tt.body)
		}
	}
}

func TestManifestDigestLegacyRehash(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	// Simulate a Phase 1 manifest without the checksums map.
	content := "legacy blob"
	sum := sha256hex(content)
	if err := h.store.Put(ctx, "blobs/sha256/"+sum, strings.NewReader(content), api.PutOpts{}); err != nil {
		t.Fatal(err)
	}
	got, err := h.pipe.manifestDigest(ctx, manifest{SHA256: sum, Size: int64(len(content)), IngestedAt: time.Now()}, "sha1")
	if err != nil {
		t.Fatalf("manifestDigest: %v", err)
	}
	if got != sha1hex(content) {
		t.Errorf("rehash sha1 = %q, want %q", got, sha1hex(content))
	}
}

// selfMetadataModule points every artifact's metadata at the artifact
// itself — the shape Maven has for .pom files.
type selfMetadataModule struct{ echoModule }

func (selfMetadataModule) MetadataIntent(_ api.Feed, coord api.PackageCoordinate) (api.Intent, bool) {
	return api.Intent{
		Kind:       api.IntentArtifact,
		Coord:      coord,
		RemotePath: coord.Name,
	}, true
}

func (selfMetadataModule) ExtractMetadata(api.PackageCoordinate, []byte) (map[string]string, error) {
	return map[string]string{}, nil
}

func TestSelfReferentialMetadataDoesNotDeadlock(t *testing.T) {
	h := newHarness(t)
	h.set("lib/self.pom", "<project/>")
	up := h.upstream(0)

	req := h.request(artifactIntent("lib/self.pom"), up)
	req.Module = selfMetadataModule{}

	done := make(chan error, 1)
	go func() {
		res, err := h.pipe.Serve(context.WithoutCancel(t.Context()), req)
		if err == nil {
			_ = res.Body.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve deadlocked on a self-referential metadata intent")
	}
}
