// Package server routes feed traffic: /{format}/{feed}/... resolved from
// the current config snapshot, with the middleware chain
// auth → policy → pipeline. The feed router is rebuilt on config reload and
// swapped atomically; anonymous access is a per-feed setting.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/go-chi/chi/v5"

	"github.com/sasokolov/package-registry/core/api"
	"github.com/sasokolov/package-registry/core/auth"
	"github.com/sasokolov/package-registry/core/config"
	"github.com/sasokolov/package-registry/core/pipeline"
	"github.com/sasokolov/package-registry/core/policy"
	"github.com/sasokolov/package-registry/core/state"
)

// Options wires the server.
type Options struct {
	Logger  *slog.Logger
	Store   api.BlobStore
	DB      *state.DB // nil: no database (tokens/audit-to-db disabled)
	Metrics *pipeline.Metrics
	Manager *config.Manager
}

// Server owns the feed router, rebuilt per config snapshot.
type Server struct {
	logger  *slog.Logger
	audit   *slog.Logger
	store   api.BlobStore
	db      *state.DB
	metrics *pipeline.Metrics
	manager *config.Manager
	pipe    *pipeline.Pipeline
	runCtx  context.Context //nolint:containedctx // root ctx for lazy OIDC/JWKS caches, set once in New

	rt atomic.Pointer[runtime]
}

type runtime struct {
	router chi.Router
	authn  *auth.Authenticator
}

type feedRuntime struct {
	feed     api.Feed
	module   api.FormatModule
	chain    *policy.Chain
	upstream *pipeline.Upstream
}

// New builds the server and its initial runtime from the manager's current
// snapshot, and subscribes to reloads. ctx is the process context: it owns
// background JWKS refreshing.
func New(ctx context.Context, o Options) (*Server, error) {
	s := &Server{
		logger:  o.Logger,
		audit:   o.Logger.With("log", "audit"),
		store:   o.Store,
		db:      o.DB,
		metrics: o.Metrics,
		manager: o.Manager,
		runCtx:  ctx,
	}
	s.pipe = pipeline.New(pipeline.Options{
		Store:   o.Store,
		Lock:    lockFunc(o.DB, o.Logger),
		Logger:  o.Logger,
		Metrics: o.Metrics,
	})

	rt, err := s.buildRuntime(o.Manager.Current())
	if err != nil {
		return nil, err
	}
	s.rt.Store(rt)
	o.Manager.Subscribe(func(cfg *config.Config) {
		next, err := s.buildRuntime(cfg)
		if err != nil {
			// The manager's validate hook should have caught this; keep the
			// old runtime rather than serving a half-built one.
			s.logger.Error("runtime rebuild failed, keeping previous", "error", err)
			return
		}
		s.rt.Store(next)
		s.logger.Info("feed runtime rebuilt", "feeds", len(cfg.Feeds))
	})
	return s, nil
}

// ValidateConfig is the manager's semantic validation hook: every feed's
// format must be registered, its policy chain constructible, and the whole
// per-format feed set acceptable to modules that constrain it
// (api.FeedSetValidator).
func ValidateConfig(cfg *config.Config) error {
	var errs []error
	byFormat := make(map[string][]api.Feed)
	for _, fc := range cfg.Feeds {
		if _, ok := api.Format(fc.Format); !ok {
			errs = append(errs, fmt.Errorf("feed %s: format %q is not registered (have %v)",
				fc.Name, fc.Format, api.Formats()))
		}
		if _, err := policy.NewChain(fc.Policies); err != nil {
			errs = append(errs, fmt.Errorf("feed %s: %w", fc.Name, err))
		}
		if _, err := pipeline.NewUpstream(pipeline.UpstreamOptions{Feed: fc.Name, BaseURL: fc.Upstream}); err != nil {
			errs = append(errs, fmt.Errorf("feed %s: %w", fc.Name, err))
		}
		byFormat[fc.Format] = append(byFormat[fc.Format], fc.API())
	}
	for format, feeds := range byFormat {
		module, ok := api.Format(format)
		if !ok {
			continue
		}
		if v, ok := module.(api.FeedSetValidator); ok {
			if err := v.ValidateFeeds(feeds); err != nil {
				errs = append(errs, fmt.Errorf("format %s: %w", format, err))
			}
		}
	}
	return errors.Join(errs...)
}

// Handler returns the dynamic feed handler; the caller mounts it under /.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.rt.Load().router.ServeHTTP(w, r)
	})
}

func (s *Server) buildRuntime(cfg *config.Config) (*runtime, error) {
	var verifier *auth.TokenVerifier
	if s.db != nil {
		verifier = auth.NewTokenVerifier(auth.NewTokens(s.db).Lookup, cfg.Auth.TokenCacheTTL.Std())
	}
	oidc := auth.NewOIDC(s.runCtx, cfg.Auth.OIDC, nil)
	rt := &runtime{
		router: chi.NewRouter(),
		authn:  auth.NewAuthenticator(verifier, oidc),
	}

	for _, fc := range cfg.Feeds {
		module, ok := api.Format(fc.Format)
		if !ok {
			return nil, fmt.Errorf("feed %s: format %q is not registered", fc.Name, fc.Format)
		}
		chain, err := policy.NewChain(fc.Policies)
		if err != nil {
			return nil, fmt.Errorf("feed %s: %w", fc.Name, err)
		}
		feed := fc.API()
		feed.ExternalURL = strings.TrimSuffix(cfg.Site.ExternalURL, "/")
		fr := &feedRuntime{feed: feed, module: module, chain: chain}
		if fc.Upstream != "" {
			fr.upstream, err = pipeline.NewUpstream(pipeline.UpstreamOptions{
				Feed:    fc.Name,
				BaseURL: fc.Upstream,
				RPS:     fc.UpstreamRPS,
				Logger:  s.logger,
				Metrics: s.metrics,
			})
			if err != nil {
				return nil, fmt.Errorf("feed %s: %w", fc.Name, err)
			}
		}

		mount := "/" + fc.Format + "/" + fc.Name
		sub := chi.NewRouter()
		for _, route := range module.Routes() {
			sub.Method(route.Method, route.Pattern, s.feedHandler(rt, fr))
		}
		rt.router.Mount(mount, http.StripPrefix(mount, sub))
	}

	// Root-level protocol endpoints (e.g. /.well-known/terraform.json)
	// provided by modules with the RootRouter capability.
	feedsByFormat := make(map[string][]api.Feed)
	for _, fc := range cfg.Feeds {
		feed := fc.API()
		feed.ExternalURL = strings.TrimSuffix(cfg.Site.ExternalURL, "/")
		feedsByFormat[fc.Format] = append(feedsByFormat[fc.Format], feed)
	}
	for _, name := range api.Formats() {
		module, _ := api.Format(name)
		rootRouter, ok := module.(api.RootRouter)
		if !ok {
			continue
		}
		feeds := feedsByFormat[name]
		for _, route := range rootRouter.RootRoutes() {
			rt.router.Method(route.Method, route.Pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Protocol documents answered from config alone (invariant 11).
				w.Header().Set(api.SourceHeader, string(api.SourceLocal))
				rootRouter.ServeRoot(w, r, feeds)
			}))
		}
	}

	return rt, nil
}

// lockFunc adapts state.WithLock into a pipeline.LockFunc that degrades to
// lock-less execution when the lock backend is down (invariant 7): ingest is
// idempotent and content-addressed, so the worst case is a duplicate fetch.
func lockFunc(db *state.DB, logger *slog.Logger) pipeline.LockFunc {
	if db == nil {
		return nil
	}
	return func(ctx context.Context, key string, fn func(context.Context) error) error {
		err := db.WithLock(ctx, key, fn)
		if err != nil && errors.Is(err, state.ErrLockUnavailable) {
			logger.Warn("lock backend unavailable; proceeding without cross-replica lock",
				"key", key, "error", err)
			return fn(ctx)
		}
		return err
	}
}
