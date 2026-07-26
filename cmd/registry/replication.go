package main

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sasokolov/package-registry/core/api"
	"github.com/sasokolov/package-registry/core/config"
	"github.com/sasokolov/package-registry/core/repl"
	"github.com/sasokolov/package-registry/core/server"
	"github.com/sasokolov/package-registry/core/state"

	"github.com/prometheus/client_golang/prometheus"
)

// replication wires geo federation: the journal writer on the publish path,
// the applier and pullers, and the internal API on its own listener.
//
// Everything here is optional: with replication.enabled false the registry
// is exactly the single-site build.
type replication struct {
	manager *repl.Manager
	server  *repl.Server
	listen  string
	tlsConf *tls.Config
	logger  *slog.Logger
}

// setupReplication builds the replication stack, or returns nil when the
// config does not enable it.
func setupReplication(ctx context.Context, cfg *config.Config, db *state.DB,
	store api.BlobStore, srv *server.Server, promReg *prometheus.Registry,
	logger *slog.Logger, subscribe func(func(*config.Config))) (*replication, error) {

	rc := cfg.Replication
	if !rc.Enabled {
		return nil, nil
	}
	if db == nil {
		return nil, errors.New("replication requires a database (the journal lives in PostgreSQL)")
	}

	// Migrations run asynchronously so the read path does not depend on the
	// database (invariant 7); replication does, so it waits here.
	identity, err := awaitSiteIdentity(ctx, db, cfg.Site.Name, logger)
	if err != nil {
		return nil, err
	}
	logger.Info("geo replication enabled",
		"site", identity.Site, "site_uuid", identity.UUID, "peers", len(rc.Peers))

	metrics := repl.NewMetrics(promReg)

	// Peer clients.
	tlsConf, err := replTLSConfig(rc)
	if err != nil {
		return nil, err
	}
	// No global client timeout: each call sets its own deadline (control
	// calls are short, snapshots and blob transfers are not). A blanket
	// timeout here would cap large artifacts at whatever the WAN could move
	// in a minute, and no retry would ever make progress.
	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:       tlsConf,
			ResponseHeaderTimeout: 30 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		},
	}
	clients, err := peerClients(rc, httpClient, logger)
	if err != nil {
		return nil, err
	}

	applier := repl.NewApplier(repl.ApplierOptions{
		DB: db, Site: cfg.Site.Name, Reindex: srv, Project: srv.Publisher(),
		Logger: logger, Audit: logger.With("log", "audit"),
		Metrics: metrics, MaxSkew: rc.SkewOrDefault(),
		Eager: srv.EagerFeed,
	})
	manager := repl.NewManager(repl.ManagerOptions{
		DB: db, Store: store, Site: cfg.Site.Name, Clients: clients,
		Applier: applier, Logger: logger, Metrics: metrics,
		Digests: srv.FeedDigests, Retention: rc.RetentionOrDefault(),
	})
	applier.SetBlobs(manager)

	// The publish path now writes journal entries in the same transaction.
	writer := repl.NewWriter(cfg.Site.Name)
	srv.Publisher().SetJournal(writer, func() {
		if rc.Nudge {
			manager.NudgePeers(context.WithoutCancel(ctx))
		}
	})
	// The read path may fetch hosted content from peers while lagging.
	srv.Pipeline().SetPeerSource(manager)

	authorize, err := serverAuthorizer(rc)
	if err != nil {
		return nil, err
	}
	replServer := repl.NewServer(repl.ServerOptions{
		DB: db, Store: store, Site: cfg.Site.Name, UUID: identity.UUID,
		Logger: logger, Metrics: metrics, Authorize: authorize,
		Digests: srv.FeedDigests,
		Publish: func(ctx context.Context, req repl.ForwardedPublish) (int, []byte, error) {
			return srv.ApplyForwardedPublish(ctx, req.Feed, req.Path, req.Method,
				req.Body, req.Identity, req.ProjectPath, req.Peer)
		},
	})

	// The peer set is declarative configuration and is hot-reloaded like
	// the rest of it (invariant 8). Auth material and the listener address
	// are startup-only; a change there is reported, not silently ignored.
	manager2 := manager
	subscribe(func(next *config.Config) {
		nrc := next.Replication
		if !nrc.Enabled {
			logger.Warn("replication cannot be disabled by reload; restart the process")
			return
		}
		if nrc.InternalListen != rc.InternalListen || nrc.Auth != rc.Auth {
			logger.Warn("replication listener and auth changes need a restart",
				"listen", nrc.InternalListen, "auth_type", nrc.Auth.Type)
		}
		updated, err := peerClients(nrc, httpClient, logger)
		if err != nil {
			logger.Error("keeping the previous peer set: new one is invalid", "error", err)
			return
		}
		manager2.SetPeers(ctx, updated)
		logger.Info("replication peer set reloaded", "peers", len(updated))
	})

	return &replication{
		manager: manager, server: replServer,
		listen: rc.InternalListen, tlsConf: serverTLSConfig(tlsConf), logger: logger,
	}, nil
}

// peerClients builds one client per configured peer.
func peerClients(rc config.ReplicationConfig, httpClient *http.Client, logger *slog.Logger) ([]*repl.Client, error) {
	clients := make([]*repl.Client, 0, len(rc.Peers))
	for _, p := range rc.Peers {
		authz, err := peerAuthorizer(rc, p)
		if err != nil {
			return nil, err
		}
		clients = append(clients, repl.NewClient(repl.Peer{
			Name:         p.Name,
			URL:          strings.TrimSuffix(p.URL, "/"),
			PullInterval: p.PullInterval.Std(),
		}, httpClient, authz, logger))
	}
	return clients, nil
}

// awaitSiteIdentity retries until the schema exists (the migration loop is
// still running) or the context ends.
func awaitSiteIdentity(ctx context.Context, db *state.DB, site string, logger *slog.Logger) (state.SiteIdentity, error) {
	backoff := 500 * time.Millisecond
	for {
		identity, err := db.EnsureSiteIdentity(ctx, site)
		if err == nil {
			return identity, nil
		}
		if ctx.Err() != nil {
			return state.SiteIdentity{}, ctx.Err()
		}
		// A mismatched site name is a configuration error, not a transient
		// one: fail immediately rather than retrying forever.
		if strings.Contains(err.Error(), "mismatched site identity") {
			return state.SiteIdentity{}, err
		}
		logger.Warn("waiting for the replication schema", "error", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return state.SiteIdentity{}, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 10*time.Second {
			backoff *= 2
		}
	}
}

// Run starts the pull loops and the internal listener.
func (r *replication) Run(ctx context.Context) error {
	go r.manager.Run(ctx)

	srv := &http.Server{
		Handler:           r.server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         r.tlsConf,
	}
	ln, err := net.Listen("tcp", r.listen)
	if err != nil {
		return fmt.Errorf("replication listener %s: %w", r.listen, err)
	}
	r.logger.Info("replication API listening", "addr", ln.Addr().String(), "tls", r.tlsConf != nil)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if r.tlsConf != nil {
		err = srv.ServeTLS(ln, "", "")
	} else {
		err = srv.Serve(ln)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// replTLSConfig builds the mTLS configuration used both to call peers and
// to authenticate them.
func replTLSConfig(rc config.ReplicationConfig) (*tls.Config, error) {
	if rc.Auth.Type != "mtls" {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(rc.Auth.CertFile, rc.Auth.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load replication certificate: %w", err)
	}
	caPEM, err := os.ReadFile(rc.Auth.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read replication CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("replication CA %s contains no certificates", rc.Auth.CAFile)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// serverTLSConfig turns the shared config into a server-side one that
// demands and verifies client certificates.
func serverTLSConfig(base *tls.Config) *tls.Config {
	if base == nil {
		return nil
	}
	conf := base.Clone()
	conf.ClientAuth = tls.RequireAndVerifyClientCert
	return conf
}

// peerAuthorizer decorates outgoing requests with this site's credential.
func peerAuthorizer(rc config.ReplicationConfig, p config.PeerConfig) (func(*http.Request), error) {
	if rc.Auth.Type != "bearer" {
		// mTLS: the transport authenticates, no header needed.
		return func(*http.Request) {}, nil
	}
	tokenFile := p.TokenFile
	if tokenFile == "" {
		tokenFile = rc.Auth.TokenFile
	}
	token, err := config.ReadToken(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("peer %s: %w", p.Name, err)
	}
	return func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Registry-Peer", p.Name)
	}, nil
}

// serverAuthorizer authenticates incoming peer requests and returns the
// peer's name. Bearer mode compares in constant time; mTLS trusts the
// verified certificate's common name.
func serverAuthorizer(rc config.ReplicationConfig) (func(*http.Request) (string, error), error) {
	switch rc.Auth.Type {
	case "mtls":
		return func(r *http.Request) (string, error) {
			if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
				return "", errors.New("no verified client certificate")
			}
			return r.TLS.PeerCertificates[0].Subject.CommonName, nil
		}, nil
	case "bearer":
		expected, err := config.ReadToken(rc.Auth.TokenFile)
		if err != nil {
			return nil, err
		}
		return func(r *http.Request) (string, error) {
			header := r.Header.Get("Authorization")
			presented := strings.TrimPrefix(header, "Bearer ")
			if presented == header || subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) != 1 {
				return "", errors.New("invalid replication credential")
			}
			peer := r.Header.Get("X-Registry-Peer")
			if peer == "" {
				peer = "unknown"
			}
			return peer, nil
		}, nil
	default:
		return nil, fmt.Errorf("unsupported replication auth type %q", rc.Auth.Type)
	}
}

// makeForwarder proxies a publish to the feed's home site over the
// replication channel, where this site is an authenticated peer. The
// client's own credential never leaves this site: the publisher travels as
// an on-behalf-of identity that the home site re-authorizes.
func makeForwarder(manager *repl.Manager, logger *slog.Logger) server.ForwardFunc {
	return func(ctx context.Context, site, feed, path, method string,
		body io.ReadCloser, identity api.Identity) (int, []byte, error) {
		status, respBody, err := manager.ForwardPublish(ctx, site, feed, path, method,
			body, identity.String(), identity.ProjectPath)
		if err != nil {
			return 0, nil, err
		}
		logger.Debug("publish forwarded", "site", site, "feed", feed, "status", status)
		return status, respBody, nil
	}
}
