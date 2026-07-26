// Package repl implements geo replication: sites converge by exchanging an
// append-only journal of events over an authenticated internal API
// (docs/geo-replication.md).
//
// Design rules this package enforces:
//
//   - only facts cross the WAN — manifests, blob availability, revocations,
//     quarantine; indexes and proxy caches are rebuilt locally (invariant 15);
//   - replication can only revoke authority, never create it (invariant 14);
//   - apply is idempotent and order-independent, so any delivery order
//     converges to identical state;
//   - concurrent publishes of one coordinate resolve by rule K1: the
//     canonical state is the lexicographically smallest sha256, the
//     coordinate is quarantined and an operator resolves it explicitly —
//     bytes are never silently swapped (invariant 4).
package repl

// Event kinds. Schema version 1.
const (
	// KindManifestPut announces a published coordinate.
	KindManifestPut = "manifest_put"
	// KindBlobAvailable announces that a blob can be fetched from a site.
	KindBlobAvailable = "blob_available"
	// KindTokenRevoke revokes a static token by hash. There is deliberately
	// no token-create event (invariant 14).
	KindTokenRevoke = "token_revoke"
	// KindQuarantineSet quarantines a coordinate.
	KindQuarantineSet = "quarantine_set"
	// KindQuarantineRelease lifts a quarantine.
	KindQuarantineRelease = "quarantine_release"
	// KindConflictResolve records an operator's choice for a conflicted
	// coordinate.
	KindConflictResolve = "conflict_resolve"
)

// SchemaVersion of the event payloads in this build.
const SchemaVersion = 1

// ManifestPut announces one published coordinate. Everything needed to
// serve it is here; the blob itself is fetched by digest.
type ManifestPut struct {
	Feed      string            `json:"feed"`
	Path      string            `json:"path"`
	Coord     string            `json:"coordinate"`
	SHA256    string            `json:"sha256"`
	Size      int64             `json:"size"`
	Checksums map[string]string `json:"checksums,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Mutable   bool              `json:"mutable,omitempty"`
	Publisher string            `json:"published_by"`
}

// BlobAvailable announces that a site holds a blob. Peers use it to fetch
// eagerly; lazy feeds ignore it and fetch on demand.
type BlobAvailable struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// TokenRevoke revokes a static token everywhere. Only the hash travels.
type TokenRevoke struct {
	Hash string `json:"hash"`
}

// QuarantineSet blocks a coordinate from being served.
type QuarantineSet struct {
	Feed       string `json:"feed"`
	Coordinate string `json:"coordinate"`
	Reason     string `json:"reason"`
	Detail     string `json:"detail,omitempty"`
}

// QuarantineRelease lifts one quarantine reason. Reasons are independent:
// releasing a manual takedown must not clear a cross-site conflict.
type QuarantineRelease struct {
	Feed       string `json:"feed"`
	Coordinate string `json:"coordinate"`
	Reason     string `json:"reason"`
}

// ConflictResolve is an operator decision for a conflicted coordinate.
type ConflictResolve struct {
	Feed     string `json:"feed"`
	Path     string `json:"path"`
	Coord    string `json:"coordinate"`
	KeepSHA  string `json:"keep_sha256"`
	Operator string `json:"operator"`
}

// Tombstone kinds are reserved for user-facing deletes (legal takedowns).
// They are not emitted or applied yet; declaring them keeps the protocol
// forward-compatible, and an applier that meets an unknown kind parks it.
const (
	KindManifestDelete = "manifest_delete"
	KindBlobTombstone  = "blob_tombstone"
)
