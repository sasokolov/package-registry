package adminapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// APIPrefix is where the registry's own API lives. It is a reserved prefix:
// no format module and no feed may claim it, or a package path could shadow
// the console's own endpoints.
const APIPrefix = "/api/v1"

// Handler returns the API router.
//
// The split is deliberate. Read endpoints that only expose what a feed
// already serves are open to whoever may read that feed; everything that
// names configuration, credentials or another site is administrator-only,
// checked per handler rather than by a blanket middleware — a middleware
// that has to be remembered is a middleware that will be forgotten.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()

	// Read surface.
	r.Get("/status", s.handleStatus)
	r.Get("/whoami", s.handleWhoAmI)
	r.Get("/feeds", s.handleFeeds)
	r.Get("/feeds/{feed}/packages", s.handlePackages)
	r.Get("/feeds/{feed}/packages/*", s.handlePackage)
	r.Get("/replication", s.handleReplication)
	r.Get("/conflicts", s.handleListConflicts)
	r.Get("/quarantine", s.handleListQuarantine)

	// Operator actions.
	r.Post("/quarantine", s.handleQuarantine)
	r.Post("/conflicts/resolve", s.handleResolveConflict)
	r.Get("/tokens", s.handleListTokens)
	r.Post("/tokens", s.handleCreateToken)
	r.Delete("/tokens/{name}", s.handleRevokeToken)

	// Configuration as a resource.
	r.Get("/config", s.handleGetConfig)
	r.Put("/config", s.handlePutConfig)
	r.Get("/config/feeds", s.handleListFeeds)
	r.Get("/config/feeds/{feed}", s.handleGetFeed)
	r.Put("/config/feeds/{feed}", s.handlePutFeed)
	r.Delete("/config/feeds/{feed}", s.handleDeleteFeed)
	r.Get("/config/admins", s.handleGetAdmins)
	r.Put("/config/admins", s.handlePutAdmins)
	r.Get("/config/peers", s.handleGetPeers)
	r.Put("/config/peers/{peer}", s.handlePutPeer)
	r.Delete("/config/peers/{peer}", s.handleDeletePeer)

	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		s.writeError(w, http.StatusNotFound, "no such endpoint")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed on this endpoint")
	})
	return r
}
