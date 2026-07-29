package server

import (
	"net/http"

	"github.com/fondaco-dev/fondaco/core/access"
	"github.com/fondaco-dev/fondaco/core/api"
	"github.com/fondaco-dev/fondaco/core/config"
)

// Access checks, in one place so every path through the server asks the same
// question of the same engine.

// accessIdentity translates an authenticated caller into what the access
// engine matches on. The engine takes its own type so it stays a leaf
// package; this is the one place the two meet.
func accessIdentity(id api.Identity) access.Identity {
	return access.Identity{
		Kind:        string(id.Kind),
		Subject:     id.Subject,
		Issuer:      id.Issuer,
		ProjectPath: id.ProjectPath,
		Ref:         id.Ref,
	}
}

// capabilityFor says which capability an intent needs. Reading a coordinate
// is a read; asking what exists is a list.
func capabilityFor(intent api.Intent) access.Capability {
	if intent.Kind == api.IntentSearch {
		return access.CapList
	}
	return access.CapRead
}

// mayServe reports whether id may have this intent answered from this feed,
// and explains the answer.
func (rt *runtime) mayServe(id api.Identity, feed string, intent api.Intent) access.Decision {
	return rt.access.Explain(accessIdentity(id),
		config.FeedPath(feed, intent.Coord.String()), capabilityFor(intent))
}

// mayPublish reports whether id may publish this coordinate into this feed.
//
// The coordinate is part of the question, which is the point: "may publish
// into releases" and "may publish com.example into releases" are different
// permissions, and only the second one is safe to give a team.
func (rt *runtime) mayPublish(id api.Identity, feed, coordinate string) access.Decision {
	return rt.access.Explain(accessIdentity(id), config.FeedPath(feed, coordinate), access.CapPublish)
}

// mayPublishSomething is the cheap gate before an upload body is read: it
// asks whether this identity could publish anything at all into the feed.
// The binding decision is made per coordinate, once the module has parsed
// enough to know what is being published.
func (rt *runtime) mayPublishSomething(id api.Identity, feed string) bool {
	return rt.access.MayReach(accessIdentity(id), config.FeedPath(feed, ""), access.CapPublish)
}

// writeAccessError answers a refusal, saying which rule refused.
//
// The explanation goes to the client as well as to the log. A registry that
// says only "forbidden" turns every permission question into a support
// ticket, and the reasoning is not a secret: it is the operator's own
// configuration, and the caller already knows what they tried to do.
func (s *Server) writeAccessError(w http.ResponseWriter, id api.Identity, d access.Decision) {
	if id.IsAnonymous() {
		// A stranger may simply have credentials to offer, and 401 is how
		// every client knows to offer them.
		s.writeError(w, api.ErrUnauthorized, d.Reason)
		return
	}
	s.writeError(w, api.ErrForbidden, d.Reason)
}
