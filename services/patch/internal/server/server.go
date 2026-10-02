// Package server builds and runs the service's two HTTP listeners.
//
// Patch is public by design: the live manifest and its blobs are fetched by every game
// client, so unlike Config there is no token guarding the listener as a whole. Staff
// tokens will guard the dev and staging manifest routes when PAT-B6 lands, and the
// verifier that will check them is constructed in main today and counted by
// readiness.
//
// The internal listener carries /healthz, /readyz, /metrics and pprof, and is never
// routed to from outside the Docker network — it has no authentication of its own, so
// that isolation is the only thing keeping it private. Both listeners start together
// and shut down together against a single deadline, so a SIGTERM cannot leave one
// draining while the other has already closed.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"

	"github.com/otomo-live/otomo/services/patch/internal/api"
	"github.com/otomo-live/otomo/services/patch/internal/auth"
	"github.com/otomo-live/otomo/services/patch/internal/blob"
	"github.com/otomo-live/otomo/services/patch/internal/config"
	"github.com/otomo-live/otomo/services/patch/internal/manifest"
	"github.com/otomo-live/otomo/services/patch/internal/servicekey"
)

// readHeaderTimeout bounds how long a client may take to send request headers. It is
// deliberately not configurable and deliberately much shorter than cfg.ReadTimeout:
// it is the slowloris guard, and a per-request read budget generous enough to be
// useful to real handlers is far too generous to also serve as one.
const readHeaderTimeout = 5 * time.Second

// Deps is everything the server needs from the outside. Only Logger has a fallback
// (slog.Default), so the zero value starts a server that reports itself unready —
// which is what lets the tests run one with no database, no blob volume and no
// manifests.
//
// ManifestHolder is the in-memory set the manifest route reads. When it is nil the
// route is not registered at all, so a server assembled without manifests answers the
// COM-5 catch-all rather than a 503 for a route it cannot possibly serve.
//
// Verifier re-checks staff tokens on dev and staging. It may be nil while PHP Admin
// Auth is not configured; the route then rejects every restricted-channel request with
// the verifier's own "invalid" reason rather than trusting the gateway.
//
// BlobRoot is the read-only view of Config's blob volume. When it is nil the blob
// route is not registered at all, so a zero-value Deps keeps answering every public
// request through the COM-5 catch-all.
type Deps struct {
	Ready          func(ctx context.Context) error
	Manifests      func() error
	ManifestHolder *manifest.Holder
	// ServiceKeys are the D4 keys the internal listener accepts. Nil accepts nobody:
	// every internal route answers 401.
	ServiceKeys *servicekey.Keys
	Verifier    *auth.Verifier
	BlobRoot    *blob.Root
	Version     string
	Logger      *slog.Logger
}

// Server owns the two listeners and the metrics registry behind them. Build it with
// New and run it with Run.
type Server struct {
	cfg     config.Config
	deps    Deps
	log     *slog.Logger
	metrics *metrics
	ready   atomic.Bool

	started      chan struct{}
	mu           sync.Mutex
	publicAddr   net.Addr
	metricsAddr  net.Addr
	internalAddr net.Addr
}

// New returns a Server with its metrics registry already built and populated. The
// ready flag starts false, so /readyz answers 503 until the caller has finished its
// own start-up checks and calls SetReady(true).
func New(cfg config.Config, deps Deps) *Server {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		cfg:     cfg,
		deps:    deps,
		log:     log,
		metrics: newMetrics(deps.Version),
		started: make(chan struct{}),
	}
}

// SetReady flips the flag /readyz consults. Call it once every start-up dependency has
// been verified; never call it before Run, or the service will report ready while
// nothing is listening.
func (s *Server) SetReady(ready bool) {
	s.ready.Store(ready)
}

// Started returns a channel that is closed once both listeners are bound and their
// addresses are recorded. Tests wait on it instead of polling or sleeping, which is
// what keeps them from being flaky on a slow machine.
func (s *Server) Started() <-chan struct{} {
	return s.started
}

// PublicAddr returns the address the public listener is bound to. It is the configured
// address except when that asked for port 0, which is how a test gets a free port; the
// result is only meaningful once Started is closed.
func (s *Server) PublicAddr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.publicAddr
}

// InternalAddr returns the address the internal API listener (PATCH_INTERNAL_ADDR) is
// bound to, with the same port-0 caveat as PublicAddr.
func (s *Server) InternalAddr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.internalAddr
}

// MetricsAddr returns the address the metrics listener (health, metrics and pprof) is
// bound to, with the same port-0 caveat as PublicAddr.
func (s *Server) MetricsAddr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.metricsAddr
}

// Run binds both listeners and serves until ctx is cancelled, then shuts them down
// gracefully within cfg.ShutdownTimeout and returns.
//
// Binding happens before the goroutines start, so a port already in use is reported as
// a plain error rather than a listener that dies asynchronously. Cancelling ctx stops
// accepting new connections but lets in-flight requests finish, which is what makes a
// rolling deploy drop no requests.
func (s *Server) Run(ctx context.Context) error {
	publicLn, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on PATCH_LISTEN_ADDR %q: %w", s.cfg.ListenAddr, err)
	}
	metricsLn, err := net.Listen("tcp", s.cfg.MetricsAddr)
	if err != nil {
		_ = publicLn.Close()
		return fmt.Errorf("listen on PATCH_METRICS_ADDR %q: %w", s.cfg.MetricsAddr, err)
	}
	internalLn, err := net.Listen("tcp", s.cfg.InternalAddr)
	if err != nil {
		_ = publicLn.Close()
		_ = metricsLn.Close()
		return fmt.Errorf("listen on PATCH_INTERNAL_ADDR %q: %w", s.cfg.InternalAddr, err)
	}

	publicSrv := &http.Server{
		Handler:           s.publicHandler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       s.cfg.ReadTimeout,
		WriteTimeout:      s.cfg.WriteTimeout,
		IdleTimeout:       s.cfg.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	metricsSrv := &http.Server{
		Handler:           s.internalHandler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       s.cfg.ReadTimeout,
		WriteTimeout:      s.cfg.WriteTimeout,
		IdleTimeout:       s.cfg.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	internalSrv := &http.Server{
		Handler:           s.internalAPIHandler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       s.cfg.ReadTimeout,
		WriteTimeout:      s.cfg.WriteTimeout,
		IdleTimeout:       s.cfg.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}

	s.mu.Lock()
	s.publicAddr = publicLn.Addr()
	s.metricsAddr = metricsLn.Addr()
	s.internalAddr = internalLn.Addr()
	s.mu.Unlock()
	close(s.started)

	g, groupCtx := errgroup.WithContext(ctx)

	g.Go(func() error {
		<-groupCtx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ShutdownTimeout)
		defer cancel()

		s.log.Info("shutting down", slog.Duration("timeout", s.cfg.ShutdownTimeout))

		// All listeners shut down against one shared deadline, so none waits out
		// another's in-flight requests.
		errs := make(chan error, 3)
		go func() { errs <- publicSrv.Shutdown(shutdownCtx) }()
		go func() { errs <- metricsSrv.Shutdown(shutdownCtx) }()
		go func() { errs <- internalSrv.Shutdown(shutdownCtx) }()
		return errors.Join(<-errs, <-errs, <-errs)
	})
	g.Go(func() error {
		if err := internalSrv.Serve(internalLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("internal listener: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		if err := publicSrv.Serve(publicLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("public listener: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		if err := metricsSrv.Serve(metricsLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("metrics listener: %w", err)
		}
		return nil
	})

	return g.Wait()
}

// internalAPIHandler assembles the internal listener (CF-3): the server
// manifest and blobs, for internal callers only. No gateway routes to this listener, and
// every route on it also requires a D4 service key, so a request that somehow reaches it
// without one is still refused. Unknown paths get the COM-5 404.
//
// The blob route validates its hash itself (api.Blob), and the two patterns have
// different literal second segments, so they cannot conflict.
func (s *Server) internalAPIHandler() http.Handler {
	mux := http.NewServeMux()
	if s.deps.ManifestHolder != nil {
		mux.Handle("GET /internal/patch/server-manifest/{channel}",
			s.deps.ServiceKeys.Require(servicekey.RoleSession, api.ServerManifest(s.deps.ManifestHolder)))
	}
	if s.deps.BlobRoot != nil {
		mux.Handle("GET /internal/patch/blob/{sha256}",
			s.deps.ServiceKeys.Require(servicekey.RoleSession, api.Blob(s.deps.BlobRoot, nil)))
	}
	mux.Handle("/", http.HandlerFunc(api.NotFound))

	var h http.Handler = mux
	h = s.withRecover(h)
	h = s.withAccessLog(h)
	h = withRequestID(h)
	return h
}

// publicHandler assembles the public mux and its middleware chain.
//
// Middleware is wrapped innermost-first, so a request passes through request ID, then
// the access log, then metrics, then panic recovery, and finally the router.
//
// The blob route is registered only once a blob root has been supplied, so a
// zero-value Deps still answers every public request through the COM-5 catch-all. The
// catch-all is unauthenticated on purpose: a 401 for an unknown path would leak the
// shape of the route table.
//
// The blob handler lives on its own mux and is reached through api.BlobPrefix, which
// validates the path before ServeMux can clean and 307-redirect a traversal attempt.
// The separate mux is load-bearing: registering the blob wildcard beside the manifest
// wildcard on the main mux would conflict (both match /patch/v1/blob/manifest) and
// panic at start-up.
func (s *Server) publicHandler() http.Handler {
	mux := http.NewServeMux()
	if s.deps.ManifestHolder != nil {
		mux.Handle("GET /patch/v1/{channel}/manifest",
			api.Manifest(s.deps.ManifestHolder, s.deps.Verifier, s.metrics))
	}
	mux.Handle("/", http.HandlerFunc(api.NotFound))

	var h http.Handler = mux
	if s.deps.BlobRoot != nil {
		blobs := http.NewServeMux()
		blobs.Handle("GET /patch/v1/blob/{sha256}", api.Blob(s.deps.BlobRoot, s.metrics))
		h = api.BlobPrefix(s.metrics, blobs, h)
	}
	h = s.withRecover(h)
	h = s.metrics.middleware(h)
	h = s.withAccessLog(h)
	h = withRequestID(h)
	return h
}

// internalHandler assembles the internal mux: health, readiness, Prometheus metrics
// and pprof, with no authentication and no access logging.
//
// Nothing here is routed through Gateway, and the port is not published in the
// container image. That isolation is the whole access control story — pprof and
// /metrics both leak more than they should if this listener is ever exposed.
func (s *Server) internalHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", api.Healthz)
	mux.Handle("GET /readyz", api.Readyz(s.readiness))
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.metrics.registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}

// readiness reports whether the service can serve traffic: the startup flag must be
// set, then the injected dependency checks must pass. It is called once per /readyz
// request, so the work behind each check must be cheap; main composes Ready from a
// cached Postgres ping, a stat of the blob root and the verifier's in-memory key
// count, and Manifests from the in-memory manifest set for exactly that reason.
//
// Ready is checked before Manifests because a database that is down is the more
// fundamental failure, and because no manifest can be loaded without it.
//
// Manifests is the in-memory readiness gate main supplies from the manifest Holder:
// until the first LoadAll publishes a set, /readyz is 503 with "manifests not loaded",
// because a service that cannot name the current release for a single channel has
// nothing useful to tell a client, and reporting ready would send it traffic it can
// only answer with 404s. A reload that fails leaves the previous set in place, so this
// check only goes false before the first success.
//
// Both checks are optional so the zero-value Deps produces a server that reports
// itself unready rather than panicking.
func (s *Server) readiness(ctx context.Context) error {
	if !s.ready.Load() {
		return errors.New("service is still starting up")
	}
	if s.deps.Ready != nil {
		if err := s.deps.Ready(ctx); err != nil {
			return err
		}
	}
	if s.deps.Manifests != nil {
		if err := s.deps.Manifests(); err != nil {
			return err
		}
	}
	return nil
}
