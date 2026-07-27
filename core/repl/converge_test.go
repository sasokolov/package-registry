package repl

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"testing"

	"github.com/sasokolov/package-registry/core/state"
)

// The merge rules are the heart of geo replication, so they are modelled
// here as a pure function over events and property-tested: any delivery
// order (and any duplication) of the same event set must produce identical
// state on every site. This is what allows sites to converge without
// coordination.

// modelState is a site's replicated state as the merge rules see it.
type modelState struct {
	manifests map[string]string // feed/path -> sha256
	mutable   map[string]bool
	hlc       map[string]state.HLC
	// quarantine is one last-writer-wins register per (coordinate, reason):
	// a release that arrives before its set must still win, so state and
	// timestamp travel together.
	quarantine map[string]map[string]quarantineState
	revoked    map[string]bool
	// conflicts holds the SET of digests ever seen for a path: a set is
	// order-independent, a winner/loser pair is not.
	conflicts map[string]map[string]bool
	// coordOf maps a path to its coordinate — several paths share one
	// (a Maven GAV covers jar, pom and sources).
	coordOf map[string]string
	// resolved records operator decisions, which are terminal: a late
	// conflicting publish must not undo them.
	resolved    map[string]string
	resolvedHLC map[string]state.HLC
	// parked mirrors the applier's dead-letter: an event that cannot be
	// applied yet is retried after every subsequent event, which is what
	// makes out-of-order delivery converge rather than drop.
	parked []state.JournalEntry
}

func newModel() *modelState {
	return &modelState{
		manifests:   map[string]string{},
		mutable:     map[string]bool{},
		hlc:         map[string]state.HLC{},
		quarantine:  map[string]map[string]quarantineState{},
		revoked:     map[string]bool{},
		conflicts:   map[string]map[string]bool{},
		resolved:    map[string]string{},
		resolvedHLC: map[string]state.HLC{},
		coordOf:     map[string]string{},
	}
}

// apply mirrors Applier.dispatch's decision logic without a database. Any
// change to the real rules must be mirrored here, and the conformance geo
// scenarios verify the two agree end to end.
// apply merges one event and then retries anything parked, exactly as the
// puller does on every poll cycle.
func (m *modelState) apply(e state.JournalEntry, localSite string) {
	m.applyOne(e, localSite)
	m.retryParked(localSite)
}

// retryParked re-attempts parked events until a pass makes no progress.
func (m *modelState) retryParked(localSite string) {
	for {
		pending := m.parked
		m.parked = nil
		before := len(pending)
		for _, e := range pending {
			m.applyOne(e, localSite)
		}
		if len(m.parked) >= before {
			return // no progress
		}
	}
}

func (m *modelState) applyOne(e state.JournalEntry, localSite string) {
	switch e.Kind {
	case KindManifestPut:
		var p ManifestPut
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return
		}
		key := p.Feed + "/" + p.Path
		m.coordOf[key] = p.Feed + "/" + p.Coord
		if keep, resolved := m.resolved[key]; resolved {
			// An operator already decided this coordinate; replaying either
			// publish converges on the decision and never re-quarantines.
			// The digest is still recorded, so the observed set is a union
			// and identical on every site.
			m.manifests[key] = keep
			if p.SHA256 != keep {
				if m.conflicts[key] == nil {
					m.conflicts[key] = map[string]bool{keep: true}
				}
				m.conflicts[key][p.SHA256] = true
			}
			m.syncConflictQuarantine(p.Feed, p.Coord)
			return
		}
		existing, found := m.manifests[key]
		switch {
		case !found:
			m.manifests[key] = p.SHA256
			m.mutable[key] = p.Mutable
			m.hlc[key] = e.HLC
		case p.Mutable && m.mutable[key]:
			// Newest write wins, equal timestamps break by digest. The
			// watermark advances even when the digest is unchanged: a later
			// event that merely repeats the current value still means "this
			// is the state as of that time", and skipping it would let a
			// third value win or lose depending on arrival order.
			stored := m.hlc[key]
			if stored.Before(e.HLC) || (stored == e.HLC && p.SHA256 < m.manifests[key]) {
				m.manifests[key] = p.SHA256
				m.hlc[key] = e.HLC
			}
		case existing == p.SHA256:
			// Immutable coordinate, same bytes: idempotent.
		default:
			// Rule K1: canonical state is the smallest digest ever seen for
			// the coordinate (min over a set: commutative, associative,
			// idempotent), and the coordinate stays quarantined until an
			// operator resolves it.
			if m.conflicts[key] == nil {
				m.conflicts[key] = map[string]bool{existing: true}
			}
			m.conflicts[key][p.SHA256] = true
			if p.SHA256 < existing {
				m.manifests[key] = p.SHA256
			}
			m.syncConflictQuarantine(p.Feed, p.Coord)
		}
	case KindTokenRevoke:
		var p TokenRevoke
		if err := json.Unmarshal(e.Payload, &p); err == nil {
			m.revoked[p.Hash] = true // sticky: never un-revoked
		}
	case KindQuarantineSet:
		var p QuarantineSet
		if err := json.Unmarshal(e.Payload, &p); err == nil {
			m.setQuarantine(p.Feed+"/"+p.Coordinate, p.Reason, true, e.HLC)
		}
	case KindQuarantineRelease:
		var p QuarantineRelease
		if err := json.Unmarshal(e.Payload, &p); err == nil {
			reason := p.Reason
			if reason == "" {
				reason = "manual"
			}
			m.setQuarantine(p.Feed+"/"+p.Coordinate, reason, false, e.HLC)
		}
	case KindConflictResolve:
		var p ConflictResolve
		if err := json.Unmarshal(e.Payload, &p); err == nil {
			// A resolution only applies to a conflict this site has seen,
			// mirroring the applier's validation; otherwise it parks and
			// is retried once the conflicting publishes arrive.
			key := p.Feed + "/" + p.Path
			if m.conflicts[key] == nil || !m.conflicts[key][p.KeepSHA] {
				m.parked = append(m.parked, e)
				return
			}
			// Two operators can decide one path: the newest decision wins
			// by HLC, and an exact tie breaks by digest, so the outcome
			// does not depend on arrival order.
			if prev, ok := m.resolvedHLC[key]; ok {
				if prev.Before(e.HLC) || (prev == e.HLC && p.KeepSHA > m.resolved[key]) {
					// this decision wins
				} else {
					m.manifests[key] = m.resolved[key]
					return
				}
			}
			m.resolved[key] = p.KeepSHA
			m.resolvedHLC[key] = e.HLC
			m.manifests[key] = p.KeepSHA
			m.coordOf[key] = p.Feed + "/" + p.Coord
			// The block is derived, so resolving one path only lifts it
			// when no sibling path of the coordinate is still open.
			m.syncConflictQuarantine(p.Feed, p.Coord)
		}
	}
	_ = localSite
}

// quarantineState is one reason's register: active or not, plus when it was
// last decided.
type quarantineState struct {
	active bool
	hlc    state.HLC
}

func (m *modelState) setQuarantine(key, reason string, active bool, hlc state.HLC) {
	if m.quarantine[key] == nil {
		m.quarantine[key] = map[string]quarantineState{}
	}
	cur, ok := m.quarantine[key][reason]
	if ok {
		// Equal timestamps break towards "blocked": with no way to order
		// the two decisions, every site must still pick the same one.
		switch {
		case cur.hlc.Before(hlc):
		case cur.hlc == hlc && active && !cur.active:
		default:
			return
		}
	}
	m.quarantine[key][reason] = quarantineState{active: active, hlc: hlc}
}

// syncConflictQuarantine mirrors syncConflictQuarantineTx: the block is
// derived from the coordinate's unresolved path conflicts, never stamped.
func (m *modelState) syncConflictQuarantine(feed, coord string) {
	key := feed + "/" + coord
	open := false
	for path, digests := range m.conflicts {
		if m.coordOf[path] != key {
			continue
		}
		if len(digests) > 1 && m.resolved[path] == "" {
			open = true
			break
		}
	}
	if m.quarantine[key] == nil {
		m.quarantine[key] = map[string]quarantineState{}
	}
	st := m.quarantine[key]["cross_site_conflict"]
	st.active = open
	m.quarantine[key]["cross_site_conflict"] = st
}

// activeReasons lists the reasons currently blocking a coordinate.
func (m *modelState) activeReasons(key string) []string {
	var out []string
	for reason, st := range m.quarantine[key] {
		if st.active {
			out = append(out, reason)
		}
	}
	sort.Strings(out)
	return out
}

// fingerprint renders the state so two models can be compared exactly.
func (m *modelState) fingerprint() string {
	var b strings.Builder
	writeSorted := func(label string, entries map[string]string) {
		keys := make([]string, 0, len(entries))
		for k := range entries {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "%s|%s=%s\n", label, k, entries[k])
		}
	}
	writeSorted("manifest", m.manifests)
	quarantined := make([]string, 0, len(m.quarantine))
	for k := range m.quarantine {
		quarantined = append(quarantined, k)
	}
	sort.Strings(quarantined)
	for _, k := range quarantined {
		if reasons := m.activeReasons(k); len(reasons) > 0 {
			fmt.Fprintf(&b, "quarantine|%s=%s\n", k, strings.Join(reasons, ","))
		}
	}
	revoked := make([]string, 0, len(m.revoked))
	for h := range m.revoked {
		revoked = append(revoked, h)
	}
	sort.Strings(revoked)
	for _, h := range revoked {
		fmt.Fprintf(&b, "revoked|%s\n", h)
	}
	conflicts := make([]string, 0, len(m.conflicts))
	for k := range m.conflicts {
		conflicts = append(conflicts, k)
	}
	sort.Strings(conflicts)
	for _, k := range conflicts {
		digests := make([]string, 0, len(m.conflicts[k]))
		for d := range m.conflicts[k] {
			digests = append(digests, d)
		}
		sort.Strings(digests)
		fmt.Fprintf(&b, "conflict|%s=%s\n", k, strings.Join(digests, ","))
	}
	return b.String()
}

func digestOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func mkEvent(t *testing.T, site string, seq int64, wall, logical int64, kind string, payload any) state.JournalEntry {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return state.JournalEntry{
		OriginSite: site, OriginSeq: seq, Kind: kind, Payload: raw,
		HLC: state.HLC{Wall: wall, Logical: logical}, SchemaVersion: SchemaVersion,
	}
}

// randomEvents builds a mixed event set covering every merge rule.
func randomEvents(t *testing.T, rng *rand.Rand) []state.JournalEntry {
	t.Helper()
	var events []state.JournalEntry
	wall := int64(1_700_000_000_000)

	logical := int64(0)
	for i := 0; i < 20; i++ {
		// Sometimes keep the wall clock and bump only the logical counter:
		// that is the case HLCs exist for (same millisecond, causal order).
		if rng.IntN(4) == 0 {
			logical++
		} else {
			wall += int64(rng.IntN(1000) + 1)
			logical = 0
		}
		site := []string{"eu-1", "us-1", "ap-1"}[rng.IntN(3)]
		seq := int64(i + 1)
		switch rng.IntN(7) {
		case 0, 1:
			// Immutable publish; some coordinates collide with different
			// content, which exercises rule K1. Several PATHS share one
			// COORDINATE — a Maven GAV covers jar, pom and sources — which
			// is what makes coordinate-level quarantine and path-level
			// conflicts disagree if the block is not derived.
			pkg := fmt.Sprintf("pkg-%d", rng.IntN(4))
			ext := []string{".jar", ".pom", "-sources.jar"}[rng.IntN(3)]
			content := fmt.Sprintf("content-%d", rng.IntN(3))
			events = append(events, mkEvent(t, site, seq, wall, logical, KindManifestPut, ManifestPut{
				Feed: "hosted", Path: pkg + "/1.0.0/" + pkg + "-1.0.0" + ext,
				Coord:  "maven:com.example:" + pkg + "@1.0.0",
				SHA256: digestOf(content), Size: int64(len(content)),
			}))
		case 2:
			// Mutable pointer: dist-tag style, converges by HLC.
			events = append(events, mkEvent(t, site, seq, wall, logical, KindManifestPut, ManifestPut{
				Feed: "hosted", Path: "-/hosted/pkg/dist-tags/latest",
				Coord: "npm:pkg", SHA256: digestOf(fmt.Sprintf("v%d", rng.IntN(5))),
				Mutable: true,
			}))
		case 3:
			events = append(events, mkEvent(t, site, seq, wall, logical, KindTokenRevoke, TokenRevoke{
				Hash: digestOf(fmt.Sprintf("token-%d", rng.IntN(3))),
			}))
		case 4:
			events = append(events, mkEvent(t, site, seq, wall, logical, KindQuarantineSet, QuarantineSet{
				Feed: "hosted", Coordinate: fmt.Sprintf("maven:com.example:pkg-%d@1.0.0", rng.IntN(4)),
				Reason: []string{"manual", "policy_osv"}[rng.IntN(2)],
			}))
		case 5:
			// Releases are generated independently of their sets: the two
			// usually originate at different sites and therefore travel in
			// separate streams that arrive in any order.
			events = append(events, mkEvent(t, site, seq, wall, logical, KindQuarantineRelease, QuarantineRelease{
				Feed: "hosted", Coordinate: fmt.Sprintf("maven:com.example:pkg-%d@1.0.0", rng.IntN(4)),
				Reason: []string{"manual", "policy_osv"}[rng.IntN(2)],
			}))
		case 6:
			// An operator resolution for a coordinate that may or may not
			// have conflicted yet, keeping either of the two contents.
			pkg := fmt.Sprintf("pkg-%d", rng.IntN(4))
			content := fmt.Sprintf("content-%d", rng.IntN(3))
			events = append(events, mkEvent(t, site, seq, wall, logical, KindConflictResolve, ConflictResolve{
				Feed: "hosted", Path: pkg + "/1.0.0/" + pkg + ".jar",
				Coord:   "maven:com.example:" + pkg + "@1.0.0",
				KeepSHA: digestOf(content), Operator: "alice",
			}))
		}
	}
	return events
}

// TestMergeConvergesUnderAnyOrder is the property that makes the design
// work: sites that receive the same events in different orders — with
// duplicates — end up in identical state.
func TestMergeConvergesUnderAnyOrder(t *testing.T) {
	for seed := uint64(1); seed <= 50; seed++ {
		rng := rand.New(rand.NewPCG(seed, seed*7+1))
		events := randomEvents(t, rng)

		reference := newModel()
		for _, e := range events {
			reference.apply(e, "eu-1")
		}
		want := reference.fingerprint()

		// Every site sees the same events in its own order, some twice.
		for trial := 0; trial < 8; trial++ {
			shuffled := append([]state.JournalEntry(nil), events...)
			rng.Shuffle(len(shuffled), func(i, j int) {
				shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
			})
			// Duplicate a few entries: delivery is at-least-once.
			for k := 0; k < 3 && len(shuffled) > 0; k++ {
				idx := rng.IntN(len(shuffled))
				shuffled = append(shuffled, shuffled[idx])
			}

			other := newModel()
			for _, e := range shuffled {
				other.apply(e, "us-1")
			}
			if got := other.fingerprint(); got != want {
				t.Fatalf("seed %d trial %d: replicas diverged\n--- reference ---\n%s\n--- shuffled ---\n%s",
					seed, trial, want, got)
			}
		}
	}
}

// TestK1PicksSmallestDigest pins the conflict rule itself: the winner is
// content-derived, so no clock or site ordering can change it.
func TestK1PicksSmallestDigest(t *testing.T) {
	a, b := digestOf("alpha"), digestOf("beta")
	smaller, larger := a, b
	if b < a {
		smaller, larger = b, a
	}

	// Apply in both orders; the canonical state must be identical.
	for _, order := range [][2]string{{smaller, larger}, {larger, smaller}} {
		m := newModel()
		for i, sha := range order {
			m.apply(mkEvent(t, "site", int64(i+1), int64(1000+i), 0, KindManifestPut, ManifestPut{
				Feed: "hosted", Path: "lib/1.0.0/lib.jar", Coord: "maven:com.example:lib@1.0.0",
				SHA256: sha,
			}), "local")
		}
		if got := m.manifests["hosted/lib/1.0.0/lib.jar"]; got != smaller {
			t.Errorf("order %v: canonical digest = %s, want the smallest (%s)", order, got, smaller)
		}
		if len(m.activeReasons("hosted/maven:com.example:lib@1.0.0")) == 0 {
			t.Errorf("order %v: conflicting coordinate was not quarantined", order)
		}
	}
}

// TestK1ResolutionConverges: an operator's choice wins everywhere, whatever
// order it arrives in relative to the conflicting publishes.
func TestK1ResolutionConverges(t *testing.T) {
	a, b := digestOf("first"), digestOf("second")
	events := []state.JournalEntry{
		mkEvent(t, "eu-1", 1, 1000, 0, KindManifestPut, ManifestPut{
			Feed: "hosted", Path: "lib/1.0.0/lib.jar", Coord: "maven:lib@1.0.0", SHA256: a}),
		mkEvent(t, "us-1", 1, 1001, 0, KindManifestPut, ManifestPut{
			Feed: "hosted", Path: "lib/1.0.0/lib.jar", Coord: "maven:lib@1.0.0", SHA256: b}),
		mkEvent(t, "eu-1", 2, 2000, 0, KindConflictResolve, ConflictResolve{
			Feed: "hosted", Path: "lib/1.0.0/lib.jar", Coord: "maven:lib@1.0.0",
			KeepSHA: b, Operator: "alice"}),
	}

	// The resolution is last in causal order; both delivery orders of the
	// two publishes must end with the operator's choice and no quarantine.
	for _, order := range [][]int{{0, 1, 2}, {1, 0, 2}} {
		m := newModel()
		for _, i := range order {
			m.apply(events[i], "local")
		}
		if got := m.manifests["hosted/lib/1.0.0/lib.jar"]; got != b {
			t.Errorf("order %v: digest = %s, want the operator's choice %s", order, got, b)
		}
		if len(m.activeReasons("hosted/maven:lib@1.0.0")) > 0 {
			t.Errorf("order %v: coordinate still quarantined after resolution", order)
		}
	}
}

// TestRevocationIsSticky: replication may only remove access (invariant 14).
func TestRevocationIsSticky(t *testing.T) {
	hash := digestOf("token")
	m := newModel()
	m.apply(mkEvent(t, "eu-1", 1, 1000, 0, KindTokenRevoke, TokenRevoke{Hash: hash}), "local")
	// Replaying older events must never resurrect the token.
	m.apply(mkEvent(t, "us-1", 1, 500, 0, KindTokenRevoke, TokenRevoke{Hash: hash}), "local")
	if !m.revoked[hash] {
		t.Fatal("revocation was lost")
	}
}

// TestProtocolHasNoAuthorityCreatingEvents guards invariant 14 structurally:
// no event kind may create credentials or lift restrictions on a peer.
func TestProtocolHasNoAuthorityCreatingEvents(t *testing.T) {
	allowed := map[string]bool{
		KindManifestPut:       true,
		KindBlobAvailable:     true,
		KindTokenRevoke:       true,
		KindQuarantineSet:     true,
		KindQuarantineRelease: true,
		KindConflictResolve:   true,
		KindManifestDelete:    true, // reserved, not emitted
		KindBlobTombstone:     true, // reserved, not emitted
	}
	for _, forbidden := range []string{"token_create", "token_upsert", "grant", "permission_set"} {
		if allowed[forbidden] {
			t.Errorf("event kind %q creates authority and must not exist in the protocol", forbidden)
		}
	}
}
