// Package api defines the canonical types and interfaces shared by the core
// pipeline and all modules (formats, storages, policies). It must stay free
// of external dependencies, and every type is designed to be serializable so
// a module could in principle live in another process.
package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Blob storage

// BlobInfo describes a stored blob.
type BlobInfo struct {
	Key     string
	Size    int64
	SHA256  string // hex digest of the content; empty if unknown
	ModTime time.Time
}

// PutOpts constrains a BlobStore.Put.
type PutOpts struct {
	// SHA256, if non-empty, is the expected hex digest. The store must verify
	// the written content and fail with ErrChecksumMismatch without leaving
	// a visible object on mismatch.
	SHA256 string
	// Size, if > 0, is the expected content length; 0 or negative means
	// unknown (a zero-length blob needs no size check to begin with).
	Size int64
}

// Iter is a pull iterator over a stream of items.
type Iter[T any] interface {
	// Next returns the next item; ok == false when the stream is exhausted
	// or failed. After ok == false consult Err.
	Next(ctx context.Context) (item T, ok bool)
	// Err reports the terminal error, if any, once Next returned ok == false.
	Err() error
}

// BlobStore is the storage contract implemented by storage modules.
// Writes must be atomic: a concurrent reader sees either the previous state
// or the complete new object, never a partial write. List yields keys in
// lexicographic order (anti-entropy walks rely on it).
type BlobStore interface {
	Get(ctx context.Context, key string) (io.ReadCloser, BlobInfo, error)
	Put(ctx context.Context, key string, r io.Reader, opts PutOpts) error
	Stat(ctx context.Context, key string) (BlobInfo, error)
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) (Iter[BlobInfo], error)
}

// Presigner is an optional BlobStore capability (discover via type assertion):
// producing pre-signed download URLs for redirect-mode serving.
type Presigner interface {
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// Initializer is an optional module capability: one-time startup
// initialization that needs the process context (e.g. ensuring a bucket
// exists). Called by the assembly code in cmd/registry.
type Initializer interface {
	Init(ctx context.Context) error
}

// ---------------------------------------------------------------------------
// Feeds and request intents

// Feed is the canonical descriptor of a configured feed as modules see it.
type Feed struct {
	Name      string
	Format    string
	Upstream  string // upstream base URL; empty for hosted-only feeds
	Anonymous bool   // whether unauthenticated reads are allowed
	// ExternalURL is the public base URL of this registry site (from
	// site.external_url) for protocols that must emit absolute self-URLs
	// (e.g. Terraform's X-Terraform-Get). Empty when not configured.
	ExternalURL string
}

// IntentKind classifies what a request wants from the generic pipeline.
type IntentKind string

const (
	// IntentArtifact is an immutable artifact: cached forever once ingested.
	IntentArtifact IntentKind = "artifact"
	// IntentMetadata is mutable metadata: cached with a TTL and served
	// stale-while-revalidate when the upstream is unavailable.
	IntentMetadata IntentKind = "metadata"
	// IntentSynthetic is answered by the registry itself from protocol
	// knowledge alone (no cache, no upstream) via the Synthesizer
	// capability; e.g. Terraform's 204 + X-Terraform-Get indirection.
	IntentSynthetic IntentKind = "synthetic"
)

// PackageCoordinate canonically identifies a package (or its metadata) inside
// a format. Policies and audit records operate on coordinates.
type PackageCoordinate struct {
	Format  string
	Name    string // canonical package name, e.g. "org.slf4j:slf4j-api" or "@scope/pkg"
	Version string // empty when the intent addresses version-less metadata
}

func (c PackageCoordinate) String() string {
	if c.Version == "" {
		return c.Format + ":" + c.Name
	}
	return c.Format + ":" + c.Name + "@" + c.Version
}

// Checksum is an expected content digest carried by an Intent.
type Checksum struct {
	Algo string // "sha256", "sha1" or "md5"
	Hex  string
}

// IsZero reports whether no checksum is set.
func (c Checksum) IsZero() bool { return c.Algo == "" && c.Hex == "" }

func (c Checksum) String() string { return c.Algo + ":" + c.Hex }

// ChecksumSource tells the pipeline where the protocol publishes the
// expected checksum of an artifact (e.g. Maven's sibling .sha1 files).
type ChecksumSource struct {
	Algo string
	// Path is the upstream request path of the checksum document, relative
	// to the feed's upstream base URL.
	Path string
}

// IsZero reports whether no checksum source is set.
func (c ChecksumSource) IsZero() bool { return c.Algo == "" && c.Path == "" }

// Intent is the canonical, format-agnostic meaning of a request; the generic
// pipeline operates exclusively on intents.
type Intent struct {
	Kind  IntentKind
	Coord PackageCoordinate
	// CacheTTL bounds metadata freshness; it is ignored for artifacts.
	CacheTTL time.Duration
	// RemotePath is the upstream request path (relative to the feed's
	// upstream base URL) that satisfies this intent. It also keys the cache.
	RemotePath string
	// Checksum, when set, must match the fetched content (invariant 5).
	Checksum Checksum
	// RemoteChecksum, when set, tells the pipeline to fetch the expected
	// checksum from this upstream document before ingesting the artifact.
	// A clean upstream 404 for the checksum document means the protocol
	// does not provide one for this artifact (ingest proceeds unverified).
	RemoteChecksum ChecksumSource
	// WantChecksum, when non-empty ("sha1", "md5", "sha256", "sha512"),
	// asks for the stored hex digest of the content at RemotePath as a
	// text response instead of the content itself (Maven sidecar files).
	WantChecksum string
	// Indirect marks artifacts whose real location is obtained from the
	// upstream response to RemotePath via the IndirectResolver capability
	// (e.g. Terraform's X-Terraform-Get).
	Indirect bool
	// RemoteURL, when set, is the absolute location of the artifact, used
	// instead of joining RemotePath onto the feed's upstream (Composer
	// dists live on arbitrary hosts). It originates from upstream metadata,
	// so the pipeline applies the same SSRF guard as to indirect locations.
	// RemotePath still keys the cache.
	RemoteURL string
	// ContentType, when set, is used for the response Content-Type.
	ContentType string
}

// Source labels where a response body came from; every response carries it
// in the SourceHeader header (invariant 11).
type Source string

// Response source values and the canonical header name.
const (
	SourceCache    Source = "cache"
	SourceUpstream Source = "upstream"
	SourceStale    Source = "stale"
	SourceLocal    Source = "local"
	// SourcePeer marks content fetched from a geo peer while replication
	// catches up (docs/geo-replication.md).
	SourcePeer Source = "peer"

	SourceHeader = "X-Registry-Source"
	// SiteHeader names the geo site that answered.
	SiteHeader = "X-Registry-Site"
	// GroupMemberHeader names the member feed that answered a group
	// request, so "which repository is this actually from" is one header
	// away instead of a guess.
	GroupMemberHeader = "X-Registry-Member"
	// GroupMergedHeader lists, in order, the members whose documents were
	// merged into the answer.
	GroupMergedHeader = "X-Registry-Merged"
)

// ---------------------------------------------------------------------------
// Format modules

// Route declares an HTTP route a format module serves, relative to the feed
// mount point /{format}/{feed}.
type Route struct {
	Method  string
	Pattern string // chi-style pattern, e.g. "/*" or "/{pkg}/-/{tarball}"
}

// FormatModule translates a package manager's wire protocol into Intents and
// back. Implementations must be stateless and must not touch infrastructure
// (storage, database, upstream HTTP) — that is the core's job.
type FormatModule interface {
	Name() string
	Routes() []Route
	// Parse maps a request (its URL is already relative to the feed mount
	// point) to a canonical Intent.
	Parse(r *http.Request) (Intent, error)
	// RewriteMetadata rewrites upstream metadata (URLs etc.) for serving
	// from this registry. Called by the pipeline before caching metadata.
	RewriteMetadata(feed Feed, upstreamBody []byte) ([]byte, error)
}

// PublishRequest is one hosted coordinate becoming visible. Modules stage
// blob content through Blobs() and then commit through CoreServices.Publish:
// that is the ONLY way hosted manifests are written, so the core owns
// immutability, provenance, audit and (from Phase 7) journalling.
type PublishRequest struct {
	Feed  Feed
	Coord PackageCoordinate
	// Path is the protocol path the coordinate is served at, relative to
	// the feed mount point (e.g. "com/example/lib/1.0/lib-1.0.jar").
	Path string
	// SHA256 identifies the already-staged blob (blobs/sha256/<hex>).
	SHA256 string
	Size   int64
	// Checksums are additional digests to store alongside (sha1, md5, ...).
	Checksums map[string]string
	// Identity is the publisher; recorded in provenance and audit.
	Identity Identity
	// Mutable marks coordinates that may be overwritten (SNAPSHOT
	// artifacts, feed indexes); release artifacts must leave it false so
	// invariant 4 applies.
	Mutable bool
	// Metadata carries canonical Meta* keys extracted from the payload.
	Metadata map[string]string
}

// PublishResult reports what the commit did.
type PublishResult struct {
	// Created is false when an identical publish was already recorded
	// (idempotent client retry).
	Created bool
	SHA256  string
}

// CoreServices is the facade through which hosting modules reach core
// functionality. Modules never touch storage keys, the database or the
// replication journal directly (invariant 2).
type CoreServices interface {
	// Blobs gives content-addressed staging: modules Put blobs and read
	// their own hosted content back.
	Blobs() BlobStore
	// Publish commits one coordinate; returns ErrImmutable on a conflicting
	// republish of a release.
	Publish(ctx context.Context, req PublishRequest) (PublishResult, error)
	// Manifests lists hosted coordinates of a feed under a path prefix —
	// the input of Reindex.
	Manifests(ctx context.Context, feed Feed, prefix string) ([]HostedManifest, error)
	// PutIndex stores a generated feed index (maven-metadata.xml,
	// dist-tags, versions) at path. Indexes are derived data: they are
	// rebuilt locally and never replicated (invariant 15).
	PutIndex(ctx context.Context, feed Feed, path string, body []byte) error
	// Site is this registry site's name (provenance, X-Registry-Site).
	Site() string
}

// HostedManifest is a published coordinate as Reindex sees it.
type HostedManifest struct {
	Path        string
	Coord       PackageCoordinate
	SHA256      string
	Size        int64
	Checksums   map[string]string
	Metadata    map[string]string
	Site        string
	Publisher   string
	PublishedAt time.Time
}

// Hoster is an optional FormatModule capability: hosting locally published
// packages (discover via type assertion).
type Hoster interface {
	HandlePublish(ctx context.Context, feed Feed, r *http.Request, deps CoreServices) error
	Reindex(ctx context.Context, feed Feed, deps CoreServices) error
}

// SyntheticResponse is a protocol-level response produced by a module
// without touching cache or upstream. Serializable by design.
type SyntheticResponse struct {
	Status int
	Header map[string]string
	Body   []byte
}

// Synthesizer is an optional FormatModule capability: it answers intents of
// kind IntentSynthetic (e.g. Terraform's download indirection). The server
// labels such responses X-Registry-Source: local.
type Synthesizer interface {
	Synthesize(feed Feed, intent Intent) (SyntheticResponse, error)
}

// IndirectTarget is the real artifact location named by a protocol
// indirection, plus the checksum that indirection published (if any).
type IndirectTarget struct {
	// Location is an absolute URL, or a URL relative to the indirection
	// document.
	Location string
	// Checksum, when set, must match the fetched content (invariant 5).
	Checksum Checksum
}

// IndirectResolver is an optional FormatModule capability for protocols
// where the upstream response to Intent.RemotePath is an indirection that
// names the real artifact location. Arguments are serializable on purpose.
type IndirectResolver interface {
	ResolveIndirect(feed Feed, intent Intent, status int, header map[string][]string, body []byte) (IndirectTarget, error)
}

// RedirectSafe is an optional FormatModule capability: the protocol
// tolerates a 302 to a pre-signed storage URL for artifact downloads.
// Clients that verify checksums themselves (maven, npm tarballs, terraform
// archives, nuget flat container) are redirect-safe; metadata never is.
type RedirectSafe interface {
	// RedirectSafeIntent reports whether this particular intent may be
	// answered with a redirect.
	RedirectSafeIntent(intent Intent) bool
}

// PublishRouter is an optional Hoster capability: declare the write routes
// explicitly instead of the default catch-all PUT/POST.
type PublishRouter interface {
	PublishRoutes() []Route
}

// MetadataSource is an optional FormatModule capability: it tells the core
// which document carries an artifact's metadata (Maven: the sibling .pom)
// and parses it into canonical Meta* keys, so policies never learn formats.
type MetadataSource interface {
	// MetadataIntent describes the document to fetch for coord; ok is false
	// when the format publishes no such document.
	MetadataIntent(feed Feed, coord PackageCoordinate) (intent Intent, ok bool)
	// ExtractMetadata parses that document into canonical Meta* keys for
	// the given coordinate (the document may describe many versions).
	ExtractMetadata(coord PackageCoordinate, body []byte) (map[string]string, error)
}

// ConditionalPutter is an optional BlobStore capability: create-only writes
// (S3 If-None-Match:*, POSIX O_EXCL) used to close publish races at the
// storage layer.
type ConditionalPutter interface {
	// PutIfAbsent writes key only when it does not exist yet; it returns
	// ErrImmutable when the key is already present.
	PutIfAbsent(ctx context.Context, key string, r io.Reader, opts PutOpts) error
}

// GroupPart is one member's answer to a group request, in member order.
type GroupPart struct {
	// Feed is the member feed the document came from.
	Feed string
	Body []byte
}

// GroupMerger is an optional FormatModule capability: combining the same
// document from several members of a group into one answer.
//
// A group exists so a client can point at one URL and get both what this
// site hosts and what it proxies. For an artifact that is a first-hit
// lookup, but for the documents that LIST what exists — maven-metadata.xml,
// an npm packument, a NuGet flat-container index — first-hit is silently
// wrong: the hosted member answers, and every version the upstream had
// disappears. Those documents have to be merged, and only the format knows
// how, so the core asks rather than guesses (invariant 1).
//
// A format without this capability cannot be grouped: configuration naming
// it in a group is refused. Half a version list is a worse answer than a
// clear error.
type GroupMerger interface {
	// MergeableIntent reports whether this intent names a document that
	// must be merged across members rather than answered by the first
	// member that has it.
	MergeableIntent(intent Intent) bool
	// Merge combines member documents into one body. Parts arrive in
	// member order and only for members that had the document; there is
	// always at least one.
	Merge(feed Feed, intent Intent, parts []GroupPart) ([]byte, error)
}

// FeedSetValidator is an optional FormatModule capability: validate the
// whole set of feeds configured for this format (e.g. a protocol that can
// expose only one feed per host through root-level service discovery).
// Called during config validation, so misconfigurations are rejected at
// startup and on reload instead of silently misrouting clients.
type FeedSetValidator interface {
	ValidateFeeds(feeds []Feed) error
}

// RootRouter is an optional FormatModule capability for protocol endpoints
// that must live at the server root, outside any feed mount (e.g.
// /.well-known/terraform.json). ServeRoot receives the feeds of this
// module's format from the current config snapshot.
type RootRouter interface {
	RootRoutes() []Route
	ServeRoot(w http.ResponseWriter, r *http.Request, feeds []Feed)
}

// ---------------------------------------------------------------------------
// Identity and policies

// IdentityKind discriminates how a caller was authenticated.
type IdentityKind string

// Identity kinds.
const (
	IdentityAnonymous IdentityKind = "anonymous"
	IdentityToken     IdentityKind = "token"
	IdentityOIDC      IdentityKind = "oidc"
)

// Identity is the authenticated caller as policies and audit see it.
type Identity struct {
	Kind IdentityKind
	// Subject uniquely identifies the principal: the token name for static
	// tokens, the "sub" claim for OIDC, "anonymous" otherwise.
	Subject string
	// ProjectPath and Ref carry GitLab CI OIDC claims when Kind is oidc.
	ProjectPath string
	Ref         string
	// Stale marks an identity served from cache while the token backend is
	// unreachable. Reads accept it (invariant 7); writes must not.
	Stale bool
}

// ParseIdentity reverses Identity.String(). It is used when an identity
// crosses a trust boundary as text (a publish forwarded to its home site);
// the caller must have authenticated the sender first — parsing grants
// nothing by itself.
func ParseIdentity(s string) Identity {
	kind, subject, ok := strings.Cut(s, ":")
	if !ok {
		return Identity{Kind: IdentityAnonymous, Subject: s}
	}
	id := Identity{Subject: subject}
	switch IdentityKind(kind) {
	case IdentityToken:
		id.Kind = IdentityToken
	case IdentityOIDC:
		id.Kind = IdentityOIDC
	default:
		id.Kind = IdentityAnonymous
	}
	return id
}

// Anonymous returns the identity of an unauthenticated caller.
func Anonymous() Identity {
	return Identity{Kind: IdentityAnonymous, Subject: "anonymous"}
}

// IsAnonymous reports whether the identity is unauthenticated.
func (id Identity) IsAnonymous() bool { return id.Kind == IdentityAnonymous || id.Kind == "" }

func (id Identity) String() string { return string(id.Kind) + ":" + id.Subject }

// Canonical artifact metadata keys. Format modules translate their protocol
// metadata into these keys so policies stay format-agnostic (invariant 1).
const (
	// MetaLicense is an SPDX identifier or the raw declared license name.
	MetaLicense = "license"
	// MetaPublishedAt is the upstream publication time, RFC3339.
	MetaPublishedAt = "published_at"
	// MetaEcosystem is the OSV ecosystem name ("Maven", "npm", "NuGet",
	// "Packagist", ...).
	MetaEcosystem = "ecosystem"
	// MetaChecksum is the expected artifact digest published by the format's
	// metadata document, as "<algo>:<hex>" (npm's dist.integrity/shasum).
	// The pipeline verifies ingests against it (invariant 5).
	MetaChecksum = "checksum"
)

// Artifact describes a concrete artifact for policy checks.
type Artifact struct {
	Coord    PackageCoordinate
	Size     int64
	Checksum Checksum
	// Metadata carries canonical Meta* keys when the format module can
	// provide them; always non-nil in policy calls, possibly empty.
	Metadata map[string]string
}

// Meta returns the canonical metadata value for key, or "".
func (a Artifact) Meta(key string) string { return a.Metadata[key] }

// Decision is a policy verdict.
type Decision struct {
	Allow bool
	// Policy names the policy that produced the verdict (set by the engine).
	Policy string
	// Code is a stable machine-readable reason, e.g. "not-in-allowlist".
	Code string
	// Reason is a human-readable explanation safe to return to the client.
	Reason string
}

// Allowed returns a positive decision.
func Allowed() Decision { return Decision{Allow: true} }

// Denied returns a negative decision with a machine code and readable reason.
func Denied(code, reason string) Decision {
	return Decision{Allow: false, Code: code, Reason: reason}
}

// Policy inspects operations on coordinates and artifacts. Implementations
// must be safe for concurrent use.
type Policy interface {
	// OnResolve runs before the pipeline touches cache or upstream.
	OnResolve(ctx context.Context, id Identity, c PackageCoordinate) Decision
	// OnServe runs after the artifact is known but before bytes are sent.
	OnServe(ctx context.Context, id Identity, a Artifact) Decision
	// OnPublish runs before locally published content is accepted.
	OnPublish(ctx context.Context, id Identity, a Artifact) Decision
}

// ---------------------------------------------------------------------------

// NotFoundf builds a not-found error carrying the offending key/coordinate.
func NotFoundf(format string, args ...any) error {
	return fmt.Errorf(format+": %w", append(args, ErrNotFound)...)
}
