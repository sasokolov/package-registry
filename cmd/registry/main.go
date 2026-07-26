// Command registry runs the package registry HTTP server and its CLI
// subcommands. Assembly happens here: storage/format/policy modules are
// linked via imports (modules.go) and wired through their registries.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/sasokolov/package-registry/core/api"
	"github.com/sasokolov/package-registry/core/config"
	"github.com/sasokolov/package-registry/core/pipeline"
	"github.com/sasokolov/package-registry/core/server"
	"github.com/sasokolov/package-registry/core/state"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "registry:", err)
		os.Exit(1)
	}
}

func run(args []string, logOut io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "token":
			return tokenCmd(args[1:], logOut)
		case "serve":
			args = args[1:]
		}
	}
	return serveCmd(args, logOut)
}

func serveCmd(args []string, logOut io.Writer) error {
	flags := flag.NewFlagSet("registry", flag.ContinueOnError)
	configPath := flags.String("config", "/etc/registry/config.yaml", "path to the YAML config file")
	logLevel := flags.String("log-level", "info", "log level: debug, info, warn or error")
	if err := flags.Parse(args); err != nil {
		return err
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return fmt.Errorf("parse -log-level: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(logOut, &slog.HandlerOptions{Level: level}))

	manager, err := config.NewManager(*configPath, logger, server.ValidateConfig)
	if err != nil {
		return err
	}
	cfg := manager.Current()
	// Site identity in every log record (audit included) — geo groundwork.
	logger = logger.With("site", cfg.Site.Name)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := api.NewStorage(cfg.Storage.Type, cfg.Storage.Options())
	if err != nil {
		return err
	}
	if init, ok := store.(api.Initializer); ok {
		go initLoop(ctx, init, logger)
	}

	var db *state.DB
	if cfg.Database.DSN != "" {
		db, err = state.Open(ctx, cfg.Database.DSN, logger)
		if err != nil {
			return err
		}
		defer db.Close()
		go db.MigrateLoop(ctx)
	} else {
		logger.Info("database disabled (no dsn): static tokens and publish are unavailable")
	}

	promReg := prometheus.NewRegistry()
	promReg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	siteInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "registry_site_info",
		Help: "Static info metric carrying the geo-site identity label.",
	}, []string{"site"})
	promReg.MustRegister(siteInfo)
	siteInfo.WithLabelValues(cfg.Site.Name).Set(1)
	metrics := pipeline.NewMetrics(promReg)

	srv, err := server.New(ctx, server.Options{
		Logger:  logger,
		Store:   store,
		DB:      db,
		Metrics: metrics,
		Manager: manager,
	})
	if err != nil {
		return err
	}
	go manager.Run(ctx)

	return serveHTTP(ctx, cfg, logger, promReg, srv.Handler())
}

// initLoop retries one-time storage initialization (e.g. bucket creation)
// so a temporarily unavailable backend does not prevent startup.
func initLoop(ctx context.Context, init api.Initializer, logger *slog.Logger) {
	backoff := time.Second
	for {
		err := init.Init(ctx)
		if err == nil {
			logger.Info("storage initialized")
			return
		}
		if ctx.Err() != nil {
			return
		}
		logger.Warn("storage init failed, will retry", "error", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// serveHTTP runs the HTTP server until ctx is cancelled, then shuts it down
// gracefully within cfg.Server.ShutdownTimeout.
func serveHTTP(ctx context.Context, cfg *config.Config, logger *slog.Logger, promReg *prometheus.Registry, feeds http.Handler) error {
	var ready atomic.Bool

	srv := &http.Server{
		Handler:           newRouter(&ready, logger, promReg, feeds),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Server.Listen, err)
	}
	ready.Store(true)
	logger.Info("server listening", "addr", ln.Addr().String())

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	// New readiness probes must fail while in-flight requests drain.
	ready.Store(false)
	logger.Info("shutting down", "timeout", cfg.Server.ShutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.Server.ShutdownTimeout.Std())
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	logger.Info("server stopped")
	return nil
}

// newRouter combines operational endpoints with the dynamic feed handler.
func newRouter(ready *atomic.Bool, logger *slog.Logger, promReg *prometheus.Registry, feeds http.Handler) http.Handler {
	r := chi.NewRouter()
	r.Use(requestLogger(logger))
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeText(w, http.StatusOK, "ok\n")
	})
	r.Get("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			writeText(w, http.StatusServiceUnavailable, "shutting down\n")
			return
		}
		writeText(w, http.StatusOK, "ok\n")
	})
	r.Method(http.MethodGet, "/metrics", promhttp.HandlerFor(promReg, promhttp.HandlerOpts{}))
	if feeds != nil {
		r.Handle("/*", feeds)
	}
	return r
}

func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	io.WriteString(w, body) //nolint:errcheck // nothing to do about a failed write to the client
}

// requestLogger logs every request as one JSON line. Probe and metrics
// endpoints are logged at debug level to keep the info log readable.
func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			level := slog.LevelInfo
			switch r.URL.Path {
			case "/healthz", "/readyz", "/metrics":
				level = slog.LevelDebug
			}
			logger.LogAttrs(r.Context(), level, "http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", ww.Status()),
				slog.Int("bytes", ww.BytesWritten()),
				slog.Duration("duration", time.Since(start)),
				slog.String("remote", r.RemoteAddr),
			)
		})
	}
}
