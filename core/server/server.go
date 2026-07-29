// Package server routes feed traffic: /{format}/{feed}/... resolved from
// the current config snapshot, with the middleware chain
// auth → policy → pipeline. The feed router is rebuilt on config reload and
// swapped atomically; anonymous access is a per-feed setting.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sasokolov/package-registry/core/access"
	"github.com/sasokolov/package-registry/core/adminapi"
	"github.com/sasokolov/package-registry/core/api"
	"github.com/sasokolov/package-registry/core/auth"
	"github.com/sasokolov/package-registry/core/config"
	"github.com/sasokolov/package-registry/core/pipeline"
	"github.com/sasokolov/package-registry/core/policy"
	"github.com/sasokolov/package-registry/core/repl"
	"github.com/sasokolov/package-registry/core/state"
)

// ForwardFunc proxies a publish request to another site and reports its
// response. It is provided by the replication wiring; without it,
// remotely-homed feeds answer 503 rather than accepting a write they cannot
// own (docs/geo-replication.md).
type ForwardFunc func(ctx context.Context, site, feed, path, method string,
	body io.ReadCloser, identity api.Identity) (status int, body2 []byte, err error)

// Options wires the server.
type Options struct {
	Logger  *slog.Logger
	Store   api.BlobStore
	DB      *state.DB // nil: no database (tokens/audit-to-db disabled)
	Metrics *pipeline.Metrics
	Manager *config.Manager
	// Usage counts what feeds and groups served. Optional: without it the
	// registry serves exactly the same, it just cannot say how much.
	Usage UsageSink
}

// UsageSink is told what was delivered. It is an interface so the server
// does not depend on how the numbers are kept.
type UsageSink interface {
	// Served reports one response: which feed, how it was answered, and the
	// size of what the client got.
	Served(feed, source string, size int64)
	// GroupServed reports a response a group produced, naming the member
	// that answered it.
	GroupServed(group, member, source string, size int64)
}

// Server owns the feed router, rebuilt per config snapshot.
type Server struct {
	logger  *slog.Logger
	audit   *slog.Logger
	store   api.BlobStore
	db      *state.DB
	metrics *pipeline.Metrics
	manager *config.Manager
	usage   UsageSink
	// forward proxies publishes to a feed's home site (write-affinity). It
	// is installed after construction because the replication manager needs
	// the server, so the request path reads it atomically.
	forward atomic.Pointer[ForwardFunc]
	// apiHandler serves the registry's own API, mounted ahead of the feed
	// routes so a package path can never shadow it.
	apiHandler atomic.Pointer[http.Handler]
	// console wraps the feed router with the web console, which claims the
	// site root and its own reserved prefix and passes everything else on.
	console    atomic.Pointer[func(http.Handler) http.Handler]
	pipe       *pipeline.Pipeline
	publisher  *pipeline.Publisher
	quarantine *quarantineCache
	site       string
	runCtx     context.Context //nolint:containedctx // root ctx for lazy OIDC/JWKS caches, set once in New

	rt atomic.Pointer[runtime]
}

type runtime struct {
	router chi.Router
	authn  *auth.Authenticator
	// access decides who may do what. It is built once per configuration
	// snapshot, so a reload swaps the whole engine rather than mutating one.
	access *access.Engine
	// oidc validates id_tokens and, for issuers configured as clients,
	// carries out the browser sign-in.
	oidc  *auth.OIDC
	feeds map[string]*feedRuntime
	// stop tears down this runtime's background work (the revocation
	// sweeper and the OIDC key cache). Config reloads build a new runtime,
	// so without it every reload would leak a goroutine and a poller.
	stop context.CancelFunc
}

type feedRuntime struct {
	feed        api.Feed
	module      api.FormatModule
	chain       *policy.Chain
	upstream    *pipeline.Upstream
	publishers  *auth.Publishers
	hosted      bool
	redirect    bool
	redirectTTL time.Duration
	// Geo federation (docs/geo-replication.md).
	publish      config.PublishPolicy
	peerFallback bool
	// members is non-empty for a group: the leaf feeds it serves from, in
	// order, already flattened through any nested groups.
	members []*feedRuntime
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
		usage:   o.Usage,
		runCtx:  ctx,
	}
	cfg0 := o.Manager.Current()
	s.site = cfg0.Site.Name
	s.pipe = pipeline.New(pipeline.Options{
		Store:   o.Store,
		Lock:    lockFunc(o.DB, o.Logger),
		Logger:  o.Logger,
		Metrics: o.Metrics,
		Site:    s.site,
	})
	s.publisher = pipeline.NewPublisher(pipeline.PublisherOptions{
		Store:  o.Store,
		DB:     o.DB,
		Site:   s.site,
		Logger: o.Logger,
		Audit:  s.audit,
	})
	if sink, ok := o.Usage.(pipeline.UsageSink); ok && o.Usage != nil {
		s.pipe.SetUsage(sink)
	}
	s.quarantine = newQuarantineCache(o.DB, 30*time.Second, o.Logger)

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
		previous := s.rt.Swap(next)
		s.logger.Info("feed runtime rebuilt", "feeds", len(cfg.Feeds))
		if previous != nil && previous.stop != nil {
			// In-flight requests still hold the old runtime; give them a
			// moment before tearing down its background work.
			go func() {
				select {
				case <-s.runCtx.Done():
				case <-time.After(time.Minute):
				}
				previous.stop()
			}()
		}
	})
	return s, nil
}

// reservedPrefixes are path segments the registry serves itself. A feed or
// format claiming one would shadow the console and its API — and would do
// it silently, because both are just paths.
var reservedPrefixes = map[string]string{
	"api": "the registry API",
	"ui":  "the web console",
}

// ValidateConfig is the manager's semantic validation hook: every feed's
// format must be registered, its policy chain constructible, and the whole
// per-format feed set acceptable to modules that constrain it
// (api.FeedSetValidator).
func ValidateConfig(cfg *config.Config) error {
	var errs []error
	byFormat := make(map[string][]api.Feed)
	for _, fc := range cfg.Feeds {
		if what, reserved := reservedPrefixes[fc.Format]; reserved {
			errs = append(errs, fmt.Errorf(
				"feed %s: format %q is reserved for %s", fc.Name, fc.Format, what))
		}
		if what, reserved := reservedPrefixes[fc.Name]; reserved && len(cfg.Feeds) > 0 {
			// Only a first-segment collision matters, and a feed is always
			// mounted under its format, so this is defensive rather than
			// load-bearing — but a name that reads like a reserved word is
			// worth refusing before it confuses someone.
			errs = append(errs, fmt.Errorf(
				"feed name %q is reserved for %s", fc.Name, what))
		}
		if _, ok := api.Format(fc.Format); !ok {
			errs = append(errs, fmt.Errorf("feed %s: format %q is not registered (have %v)",
				fc.Name, fc.Format, api.Formats()))
		}
		if _, err := policy.NewChain(fc.Policies, validationDeps{}); err != nil {
			errs = append(errs, fmt.Errorf("feed %s: %w", fc.Name, err))
		}
		if _, err := pipeline.NewUpstream(pipeline.UpstreamOptions{Feed: fc.Name, BaseURL: fc.Upstream}); err != nil {
			errs = append(errs, fmt.Errorf("feed %s: %w", fc.Name, err))
		}
		if fc.Hosted {
			if module, ok := api.Format(fc.Format); ok {
				if _, isHoster := module.(api.Hoster); !isHoster {
					errs = append(errs, fmt.Errorf("feed %s: format %q cannot host packages", fc.Name, fc.Format))
				}
			}
		}
		if _, err := auth.NewPublishers(fc.Publishers); err != nil {
			errs = append(errs, fmt.Errorf("feed %s: %w", fc.Name, err))
		}
		feed := fc.API()
		feed.ExternalURL = strings.TrimSuffix(cfg.Site.ExternalURL, "/")
		byFormat[fc.Format] = append(byFormat[fc.Format], feed)
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

// SetForward installs the publish-forwarding function used by feeds homed
// at another site. Without it those feeds answer 503 instead of accepting a
// write they cannot own.
func (s *Server) SetForward(fn ForwardFunc) { s.forward.Store(&fn) }

// Publisher exposes the write path so the replication wiring can attach a
// journal to it.
func (s *Server) Publisher() *pipeline.Publisher { return s.publisher }

// Pipeline exposes the read path so peer fallback can be attached.
func (s *Server) Pipeline() *pipeline.Pipeline { return s.pipe }

// ReindexFeed rebuilds a feed's indexes; the applier calls it after
// replicated manifests change a feed (invariant 15).
func (s *Server) ReindexFeed(ctx context.Context, feedName string) error {
	rt := s.rt.Load()
	fr, ok := rt.feeds[feedName]
	if !ok {
		return nil
	}
	return s.publisher.Reindex(ctx, fr.feed, fr.module)
}

// Identify resolves a request's credentials with the active runtime's
// authenticator — the same one the feed routes use, so the API and the
// download path never disagree about who is calling.
func (s *Server) Identify(r *http.Request) (api.Identity, error) {
	rt := s.rt.Load()
	if rt == nil || rt.authn == nil {
		return api.Anonymous(), nil
	}
	return rt.authn.Identify(r.Context(), r)
}

// PublishableFeeds lists the feeds an identity may publish to.
func (s *Server) PublishableFeeds(id api.Identity) []string {
	rt := s.rt.Load()
	if rt == nil {
		return nil
	}
	var out []string
	for name := range rt.feeds {
		if rt.mayPublishSomething(id, name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// EagerFeed reports whether a feed replicates blobs ahead of demand.
func (s *Server) EagerFeed(feedName string) bool {
	for _, fc := range s.manager.Current().Feeds {
		if fc.Name == feedName {
			return fc.ReplicationMode == "eager"
		}
	}
	return false
}

// FeedDigests computes per-feed manifest-set digests for divergence
// detection (invariant 16).
func (s *Server) FeedDigests(ctx context.Context) map[string]string {
	out := map[string]string{}
	if s.db == nil {
		return out
	}
	for _, fc := range s.manager.Current().Feeds {
		if !fc.Hosted {
			continue
		}
		rows, err := s.db.ListHosted(ctx, fc.Name, "")
		if err != nil {
			continue
		}
		out[fc.Name] = repl.FeedDigest(rows)
	}
	return out
}

// Access exposes the current access engine, so the registry's own API asks
// the same question the serving path does.
func (s *Server) Access() *access.Engine {
	rt := s.rt.Load()
	if rt == nil {
		return nil
	}
	return rt.access
}

// OIDC exposes the current issuer validator, so the registry's own API can
// sign a person in through the same trust the serving path uses.
func (s *Server) OIDC() *auth.OIDC {
	rt := s.rt.Load()
	if rt == nil {
		return nil
	}
	return rt.oidc
}

// SetAPI mounts the registry's own API under its reserved prefix. It is
// installed after construction because the API needs the server (for feed
// summaries and permissions) as much as the server needs it.
func (s *Server) SetAPI(h http.Handler) { s.apiHandler.Store(&h) }

// SetConsole installs the web console in front of the feed router. It must be
// called before Handler, which composes the chain once.
func (s *Server) SetConsole(wrap func(http.Handler) http.Handler) { s.console.Store(&wrap) }

// Handler returns the dynamic feed handler; the caller mounts it under /.
func (s *Server) Handler() http.Handler {
	var feeds http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.rt.Load().router.ServeHTTP(w, r)
	})
	if wrap := s.console.Load(); wrap != nil {
		feeds = (*wrap)(feeds)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The API answers first: its prefix is reserved, and letting the
		// feed router see those paths would make a package name able to
		// shadow the console.
		if api := s.apiHandler.Load(); api != nil && strings.HasPrefix(r.URL.Path, adminapi.APIPrefix) {
			http.StripPrefix(adminapi.APIPrefix, *api).ServeHTTP(w, r)
			return
		}
		feeds.ServeHTTP(w, r)
	})
}

func (s *Server) buildRuntime(cfg *config.Config) (*runtime, error) {
	runtimeCtx, stopRuntime := context.WithCancel(s.runCtx)
	var verifier *auth.TokenVerifier
	if s.db != nil {
		tokens := auth.NewTokens(s.db)
		verifier = auth.NewTokenVerifier(tokens.Lookup, cfg.Auth.TokenCacheTTL.Std())
		verifier.SetStaleWindow(cfg.Auth.StaleIdentityWindowOrDefault())
		// A revoked token must stop working within seconds, not within the
		// cache TTL — whether it was revoked here or at another geo site.
		go verifier.WatchRevocations(runtimeCtx, tokens,
			cfg.Auth.RevocationSweepOrDefault(), s.logger)
	}
	oidc := auth.NewOIDC(runtimeCtx, cfg.Auth.OIDC, nil)
	engine, err := cfg.AccessEngine()
	if err != nil {
		stopRuntime()
		return nil, fmt.Errorf("access rules: %w", err)
	}
	rt := &runtime{
		router: chi.NewRouter(),
		authn:  auth.NewAuthenticator(verifier, oidc),
		access: engine,
		oidc:   oidc,
		feeds:  map[string]*feedRuntime{},
		stop:   stopRuntime,
	}

	// Leaf feeds first: a group can only be wired once the feeds it names
	// exist as runtimes.
	for _, fc := range cfg.Feeds {
		if fc.IsGroup() {
			continue
		}
		module, ok := api.Format(fc.Format)
		if !ok {
			return nil, fmt.Errorf("feed %s: format %q is not registered", fc.Name, fc.Format)
		}
		chain, err := policy.NewChain(fc.Policies, s.policyDeps())
		if err != nil {
			return nil, fmt.Errorf("feed %s: %w", fc.Name, err)
		}
		feed := fc.API()
		feed.ExternalURL = strings.TrimSuffix(cfg.Site.ExternalURL, "/")
		publishers, err := auth.NewPublishers(fc.Publishers)
		if err != nil {
			return nil, fmt.Errorf("feed %s: %w", fc.Name, err)
		}
		fr := &feedRuntime{
			feed: feed, module: module, chain: chain,
			publishers: publishers, hosted: fc.Hosted,
			redirect:     fc.Redirect,
			redirectTTL:  fc.RedirectTTLOrDefault(),
			publish:      fc.Publish(cfg.Site.Name),
			peerFallback: fc.PeerFallback,
		}
		if fc.Redirect {
			if _, ok := s.store.(api.Presigner); !ok {
				s.logger.Warn("feed asks for redirect mode but the storage cannot pre-sign; streaming instead",
					"feed", fc.Name)
				fr.redirect = false
			} else if _, ok := module.(api.RedirectSafe); !ok {
				s.logger.Warn("feed asks for redirect mode but the format is not redirect-safe; streaming instead",
					"feed", fc.Name, "format", fc.Format)
				fr.redirect = false
			}
		}
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

		rt.feeds[fc.Name] = fr

		mount := "/" + fc.Format + "/" + fc.Name
		sub := chi.NewRouter()
		for _, route := range module.Routes() {
			sub.Method(route.Method, route.Pattern, s.feedHandler(rt, fr))
		}
		if hoster, ok := module.(api.Hoster); ok && fc.Hosted {
			for _, route := range publishRoutes(module) {
				sub.Method(route.Method, route.Pattern, s.publishHandler(rt, fr, hoster))
			}
		}
		rt.router.Mount(mount, http.StripPrefix(mount, sub))
	}

	if err := s.mountGroups(cfg, rt); err != nil {
		return nil, err
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

// policyDeps hands policies the shared verdict cache and a logger.
func (s *Server) policyDeps() api.PolicyServices {
	return policyServices{db: s.db, logger: s.logger}
}

type policyServices struct {
	db     *state.DB
	logger *slog.Logger
}

func (p policyServices) GetVerdict(ctx context.Context, namespace, key string) (string, time.Time, bool, error) {
	if p.db == nil {
		return "", time.Time{}, false, nil
	}
	return p.db.GetVerdict(ctx, namespace, key)
}

func (p policyServices) PutVerdict(ctx context.Context, namespace, key, value string) error {
	if p.db == nil {
		return nil
	}
	return p.db.PutVerdict(ctx, namespace, key, value)
}

func (p policyServices) Logger() *slog.Logger { return p.logger }

// validationDeps is the no-op services implementation used while validating
// a config: policies must be constructible without touching a database.
type validationDeps struct{}

func (validationDeps) GetVerdict(context.Context, string, string) (string, time.Time, bool, error) {
	return "", time.Time{}, false, nil
}
func (validationDeps) PutVerdict(context.Context, string, string, string) error { return nil }
func (validationDeps) Logger() *slog.Logger                                     { return slog.Default() }
