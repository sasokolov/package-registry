package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/fondaco-dev/fondaco/core/api"
	"github.com/fondaco-dev/fondaco/core/config"
	"github.com/fondaco-dev/fondaco/core/pipeline"
	"github.com/fondaco-dev/fondaco/core/policy"
	"github.com/fondaco-dev/fondaco/core/usage"
)

// A group is a read-only view over several feeds of one format, so a client
// configures one URL and gets both what this site hosts and what it proxies.
//
// Two things make it more than a loop over members. An artifact is a
// first-hit lookup, but the documents that LIST what exists must be merged,
// or the hosted member answers and every proxied version disappears — the
// classic way a group silently loses packages. And a member keeps its own
// access rules: a member the caller may not read is skipped rather than
// exposed, so a group can never widen access to what it contains.

// maxMergePart bounds one member's contribution to a merged document. These
// are index documents, not artifacts; something megabytes past this is a
// misconfiguration, and buffering it per member per request is how a
// registry runs out of memory.
const maxMergePart = 32 << 20

// memberOutcome says what happened when one member was asked.
type memberOutcome int

const (
	// memberAnswered: the member has it.
	memberAnswered memberOutcome = iota
	// memberMissing: the member simply does not have it.
	memberMissing
	// memberBlocked: the member has an opinion — quarantine or policy — and
	// that opinion must not be silently converted into "not found".
	memberBlocked
)

// memberResult is one member's answer.
type memberResult struct {
	member  *feedRuntime
	outcome memberOutcome
	result  *pipeline.Result
	// reason explains a memberBlocked outcome, for the client and the log.
	reason string
	// err is a real failure (upstream down, storage error) as opposed to a
	// clean miss.
	err error
}

// groupHandler serves one group feed.
func (s *Server) groupHandler(rt *runtime, gr *feedRuntime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		adoptDeclaredCredential(gr.module, r)
		id, err := rt.authn.Identify(ctx, r)
		if err != nil {
			s.audit.Warn("authentication rejected",
				"group", gr.feed.Name, "remote", r.RemoteAddr, "error", err)
			s.writeError(w, err, "")
			return
		}
		intent, err := gr.module.Parse(r)
		if err != nil {
			s.writeErrorText(w, err, err.Error())
			return
		}

		// The group's own rules apply to every request through it, before
		// any member is asked.
		if d := rt.mayServe(id, gr.feed.Name, intent); !d.Allowed {
			s.audit.Warn("access denied",
				"group", gr.feed.Name, "identity", id.String(),
				"coordinate", intent.Coord.String(), "reason", d.Reason)
			s.writeAccessError(w, id, d)
			return
		}

		// The group's own quarantine and policies apply to every request
		// through it, before any member is touched.
		if blocked, reason := s.quarantine.Blocked(ctx, gr.feed.Name, intent.Coord.String()); blocked {
			s.audit.Warn("quarantined coordinate requested through a group",
				"group", gr.feed.Name, "identity", id.String(),
				"coordinate", intent.Coord.String(), "reason", reason)
			s.writeError(w, api.ErrQuarantined, reason)
			return
		}
		if d := gr.chain.OnResolve(ctx, id, intent.Coord); !d.Allow {
			s.auditDeny("resolve", gr, id, intent.Coord, d)
			s.writeError(w, api.ErrForbidden, d.Reason)
			return
		}

		if intent.Kind == api.IntentSynthetic {
			// Synthesized answers come from protocol knowledge alone, so
			// the group answers them itself; no member is involved.
			s.serveSynthetic(w, gr, intent)
			return
		}

		if merger, ok := gr.module.(api.GroupMerger); ok && merger.MergeableIntent(intent) {
			s.serveMerged(ctx, rt, w, gr, merger, id, intent)
			return
		}
		s.serveFirstHit(ctx, rt, w, r, gr, id, intent)
	}
}

// serveFirstHit answers from the first member that has the thing.
func (s *Server) serveFirstHit(ctx context.Context, rt *runtime, w http.ResponseWriter, r *http.Request,
	gr *feedRuntime, id api.Identity, intent api.Intent) {
	var blocked []memberResult
	var failures []error

	for _, member := range gr.members {
		res := s.askMember(ctx, rt, member, id, intent)
		switch res.outcome {
		case memberAnswered:
			defer func() { _ = res.result.Body.Close() }()
			w.Header().Set(api.GroupMemberHeader, member.feed.Name)
			if s.tryRedirect(ctx, w, r, member, intent, res.result) {
				s.groupServed(gr, member, intent, res.result)
				s.served(member.feed.Name, intent, res.result)
				return
			}
			s.streamResult(w, member, intent, res.result)
			s.groupServed(gr, member, intent, res.result)
			s.served(member.feed.Name, intent, res.result)
			return
		case memberBlocked:
			blocked = append(blocked, res)
		case memberMissing:
			if res.err != nil {
				failures = append(failures, fmt.Errorf("%s: %w", member.feed.Name, res.err))
			}
		}
	}

	s.answerEmptyGroup(w, gr, intent, blocked, failures)
}

// serveMerged asks every member and hands the answers to the format module.
func (s *Server) serveMerged(ctx context.Context, rt *runtime, w http.ResponseWriter,
	gr *feedRuntime, merger api.GroupMerger, id api.Identity, intent api.Intent) {
	var (
		parts    []api.GroupPart
		names    []string
		blocked  []memberResult
		failures []error
	)

	for _, member := range gr.members {
		res := s.askMember(ctx, rt, member, id, intent)
		switch res.outcome {
		case memberAnswered:
			body, err := readPart(res.result)
			if err != nil {
				failures = append(failures, fmt.Errorf("%s: %w", member.feed.Name, err))
				continue
			}
			parts = append(parts, api.GroupPart{Feed: member.feed.Name, Body: body})
			names = append(names, member.feed.Name)
		case memberBlocked:
			blocked = append(blocked, res)
		case memberMissing:
			if res.err != nil {
				failures = append(failures, fmt.Errorf("%s: %w", member.feed.Name, res.err))
			}
		}
	}

	if len(parts) == 0 {
		s.answerEmptyGroup(w, gr, intent, blocked, failures)
		return
	}

	body, err := merger.Merge(gr.feed, intent, parts)
	if err != nil {
		s.logger.Error("merging a group document failed",
			"group", gr.feed.Name, "coord", intent.Coord.String(),
			"members", strings.Join(names, ","), "error", err)
		s.writeError(w, err, "")
		return
	}

	// A merged document is produced here, from documents this site already
	// had; it is not cached, because each member's copy is cached by its own
	// pipeline and merging again is cheaper than inventing a second thing to
	// invalidate.
	w.Header().Set(api.SourceHeader, string(api.SourceLocal))
	w.Header().Set(api.SiteHeader, s.site)
	w.Header().Set(api.GroupMergedHeader, strings.Join(names, ","))
	w.Header().Set("Content-Type", responseContentType(intent, nil))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	if _, err := w.Write(body); err != nil {
		s.logger.Debug("client aborted a merged download",
			"group", gr.feed.Name, "coord", intent.Coord.String(), "error", err)
	}
	// A merged document has no single member behind it, and saying it came
	// from the first one would be a lie a dashboard would repeat. It is also
	// the only group answer not counted on a member, which is what lets a
	// site total add group traffic without counting anything twice.
	if s.usage != nil {
		// A merged document is metadata, so it has no place on a
		// most-downloaded list; the group's own counters still get it.
		s.usage.GroupServed(gr.feed.Name, mergedMember, usage.SourceMerged, "", int64(len(body)))
	}
}

// mergedMember stands in for "several, merged here" in the member label of a
// group's counters.
const mergedMember = "(merged)"

// askMember runs one member's own chain for this intent: its access rule,
// its quarantine, its policies, its pipeline. A member never loses a rule by
// being inside a group.
func (s *Server) askMember(ctx context.Context, rt *runtime, member *feedRuntime,
	id api.Identity, intent api.Intent) memberResult {
	// A member that this caller could not read directly is not readable
	// through a group either. Skipping rather than refusing is deliberate:
	// the group is a view, and you see the part of it you are entitled to.
	if d := rt.mayServe(id, member.feed.Name, intent); !d.Allowed {
		return memberResult{member: member, outcome: memberMissing}
	}

	if blocked, reason := s.quarantine.Blocked(ctx, member.feed.Name, intent.Coord.String()); blocked {
		return memberResult{member: member, outcome: memberBlocked, reason: reason}
	}
	if d := member.chain.OnResolve(ctx, id, intent.Coord); !d.Allow {
		s.auditDeny("resolve", member, id, intent.Coord, d)
		return memberResult{member: member, outcome: memberBlocked, reason: d.Reason}
	}

	res, err := s.answerFromMember(ctx, member, intent)
	s.updateBreakerGauge(member)
	if err != nil {
		if errors.Is(err, api.ErrNotFound) {
			return memberResult{member: member, outcome: memberMissing}
		}
		// An upstream that is down is not the same as a member that does
		// not have the package: remember it so the group can say so if
		// nothing else answers.
		return memberResult{member: member, outcome: memberMissing, err: err}
	}

	artifact := api.Artifact{
		Coord:    intent.Coord,
		Size:     res.Size,
		Checksum: api.Checksum{Algo: "sha256", Hex: res.SHA256},
		Metadata: s.artifactMetadata(ctx, member, intent),
	}
	if d := member.chain.OnServe(ctx, id, artifact); !d.Allow {
		_ = res.Body.Close()
		s.auditDeny("serve", member, id, intent.Coord, d)
		return memberResult{member: member, outcome: memberBlocked, reason: d.Reason}
	}
	return memberResult{member: member, outcome: memberAnswered, result: res}
}

// answerFromMember gets one member's answer, by the same route a direct
// request to that member would take. A hosted member answers its own
// searches; everything else goes through the pipeline.
func (s *Server) answerFromMember(ctx context.Context, member *feedRuntime,
	intent api.Intent) (*pipeline.Result, error) {
	if intent.Kind == api.IntentSearch && member.hosted && member.upstream == nil {
		searcher, ok := member.module.(api.Searcher)
		if !ok {
			return nil, api.NotFoundf("feed %s cannot answer a search", member.feed.Name)
		}
		resp, err := searcher.Search(ctx, member.feed, intent, s.publisher)
		if err != nil {
			return nil, err
		}
		// A module says "I do not have this" with a status, not an error.
		// Handing that body to the group as an answer would let one member's
		// 404 stand for the whole group and hide the members behind it.
		switch {
		case resp.Status == http.StatusNotFound:
			return nil, api.NotFoundf("feed %s does not have %s", member.feed.Name, intent.Coord)
		case resp.Status >= 400:
			return nil, fmt.Errorf("feed %s refused the request with status %d", member.feed.Name, resp.Status)
		}
		return &pipeline.Result{
			Body:        io.NopCloser(bytes.NewReader(resp.Body)),
			Size:        int64(len(resp.Body)),
			SHA256:      digestOf(resp.Header),
			Source:      api.SourceLocal,
			ContentType: resp.Header["Content-Type"],
		}, nil
	}
	return s.pipe.Serve(ctx, pipeline.Request{
		Feed:         member.feed,
		Intent:       intent,
		Module:       member.module,
		Upstream:     member.upstream,
		PeerFallback: member.peerFallback,
	})
}

// answerEmptyGroup decides what "no member had it" means. The distinction
// matters: a coordinate every member has blocked is not missing, and an
// upstream outage is not an empty repository.
func (s *Server) answerEmptyGroup(w http.ResponseWriter, gr *feedRuntime, intent api.Intent,
	blocked []memberResult, failures []error) {
	if len(blocked) > 0 {
		reasons := make([]string, 0, len(blocked))
		for _, b := range blocked {
			reasons = append(reasons, b.member.feed.Name+": "+b.reason)
		}
		s.audit.Warn("every member that has this coordinate blocks it",
			"group", gr.feed.Name, "coordinate", intent.Coord.String(),
			"reasons", strings.Join(reasons, "; "))
		s.writeError(w, api.ErrQuarantined, strings.Join(reasons, "; "))
		return
	}
	if len(failures) > 0 {
		s.logger.Warn("no member could answer",
			"group", gr.feed.Name, "coord", intent.Coord.String(),
			"error", errors.Join(failures...))
		s.writeError(w, failures[0], "")
		return
	}
	s.writeError(w, api.ErrNotFound, "no member of "+gr.feed.Name+" has it")
}

// streamResult writes one member's answer through, labelled with where that
// member got it from.
func (s *Server) streamResult(w http.ResponseWriter, member *feedRuntime,
	intent api.Intent, res *pipeline.Result) {
	w.Header().Set(api.SourceHeader, string(res.Source))
	w.Header().Set(api.SiteHeader, s.site)
	if res.Size >= 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", res.Size))
	}
	w.Header().Set("Content-Type", responseContentType(intent, res))
	setProtocolHeaders(w, member.module, member.feed, intent, res.SHA256)
	if _, err := io.Copy(w, res.Body); err != nil {
		s.logger.Debug("client aborted a group download",
			"feed", member.feed.Name, "coord", intent.Coord.String(), "error", err)
	}
}

// digestOf reads a sha256 a module put in its own response headers, so a
// document a member produced itself is served with the same provenance as
// one that came out of the store.
func digestOf(header map[string]string) string {
	for _, value := range header {
		hex, ok := strings.CutPrefix(value, "sha256:")
		if ok && len(hex) == 64 {
			return hex
		}
	}
	return ""
}

// groupServed records that a group answered and which member did the work.
//
// The member is counted too, on its own name: "how much is this feed used"
// should include what arrived through a group, since that is what the group
// is for. The group's own row is what the group URL was asked for, and a
// group that only ever answers from one member is worth noticing.
func (s *Server) groupServed(group, member *feedRuntime, intent api.Intent, res *pipeline.Result) {
	if s.usage == nil || res == nil {
		return
	}
	s.usage.GroupServed(group.feed.Name, member.feed.Name, string(res.Source),
		downloadedCoordinate(intent), res.Size)
}

// readPart buffers one member's document for merging.
func readPart(res *pipeline.Result) ([]byte, error) {
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxMergePart+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxMergePart {
		return nil, fmt.Errorf("index document exceeds %d bytes", maxMergePart)
	}
	return body, nil
}

// groupPublishHandler explains why a group refuses writes and where to send
// them instead. A 405 with the answer in it beats a 404 that leaves the
// operator guessing which repository they were supposed to use.
func (s *Server) groupPublishHandler(gr *feedRuntime) http.HandlerFunc {
	targets := make([]string, 0, len(gr.members))
	for _, member := range gr.members {
		if member.hosted {
			targets = append(targets, member.feed.Name)
		}
	}
	message := "a group is read-only; publish to a hosted feed"
	switch len(targets) {
	case 0:
		message += ". No member of " + gr.feed.Name + " accepts publishing."
	case 1:
		message += ": " + targets[0]
	default:
		message += ", one of: " + strings.Join(targets, ", ")
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", "GET, HEAD")
		s.finishError(w, http.StatusMethodNotAllowed, message)
	}
}

// resolveMembers flattens a group's members into the leaf feeds that
// actually serve, in order and without repeats. Flattening once at build
// time keeps the request path a straight loop and makes a nested group cost
// nothing extra per request.
func resolveMembers(cfgByName map[string]groupSpec, rt *runtime, name string,
	seen map[string]bool, out []*feedRuntime) []*feedRuntime {
	spec, ok := cfgByName[name]
	if !ok {
		return out
	}
	for _, member := range spec.members {
		if seen[member] {
			continue
		}
		child, isGroup := cfgByName[member]
		if isGroup && len(child.members) > 0 {
			out = resolveMembers(cfgByName, rt, member, seen, out)
			continue
		}
		fr, ok := rt.feeds[member]
		if !ok {
			continue
		}
		seen[member] = true
		out = append(out, fr)
	}
	return out
}

// groupSpec is the configured shape of one feed, reduced to what flattening
// needs.
type groupSpec struct{ members []string }

// mountGroups builds and mounts the group feeds, after every leaf feed is
// already a runtime.
func (s *Server) mountGroups(cfg *config.Config, rt *runtime) error {
	specs := make(map[string]groupSpec, len(cfg.Feeds))
	for _, fc := range cfg.Feeds {
		if fc.IsGroup() {
			specs[fc.Name] = groupSpec{members: fc.Members}
		}
	}

	for _, fc := range cfg.Feeds {
		if !fc.IsGroup() {
			continue
		}
		module, ok := api.Format(fc.Format)
		if !ok {
			return fmt.Errorf("group %s: format %q is not registered", fc.Name, fc.Format)
		}
		chain, err := policy.NewChain(fc.Policies, s.policyDeps())
		if err != nil {
			return fmt.Errorf("group %s: %w", fc.Name, err)
		}
		feed := fc.API()
		feed.ExternalURL = strings.TrimSuffix(cfg.Site.ExternalURL, "/")

		gr := &feedRuntime{
			feed:    feed,
			module:  module,
			chain:   chain,
			members: resolveMembers(specs, rt, fc.Name, map[string]bool{}, nil),
		}
		if len(gr.members) == 0 {
			// Validation has already checked that the members exist, so
			// this means they all resolved to nothing — a group of groups
			// of nothing. Serving it would be a 404 machine.
			return fmt.Errorf("group %s: none of its members resolve to a servable feed", fc.Name)
		}
		rt.feeds[fc.Name] = gr

		mount := "/" + fc.Format + "/" + fc.Name
		sub := chi.NewRouter()
		for _, route := range module.Routes() {
			sub.Method(route.Method, route.Pattern, s.groupHandler(rt, gr))
		}
		// Writes are answered explicitly rather than falling through to a
		// confusing 404 or a route meant for reads.
		for _, route := range publishRoutes(module) {
			sub.Method(route.Method, route.Pattern, s.groupPublishHandler(gr))
		}
		rt.router.Mount(mount, http.StripPrefix(mount, sub))

		names := make([]string, 0, len(gr.members))
		for _, m := range gr.members {
			names = append(names, m.feed.Name)
		}
		s.logger.Info("group mounted",
			"group", fc.Name, "format", fc.Format, "members", strings.Join(names, ","))
	}
	return nil
}
