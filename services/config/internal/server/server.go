// Package server builds and runs the service's two HTTP listeners.
//
// The public listener carries the staff API and requires a token on every route,
// including the fallback. The internal listener carries /healthz, /readyz, /metrics
// and pprof, and is never routed to from outside the Docker network — it has no
// authentication of its own, so that isolation is the only thing keeping it private.
// Both listeners start together and shut down together against a single deadline, so
// a SIGTERM cannot leave one draining while the other has already closed.
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

	"github.com/otomo-live/otomo/services/config/internal/api"
	"github.com/otomo-live/otomo/services/config/internal/auth"
	"github.com/otomo-live/otomo/services/config/internal/config"
)

// readHeaderTimeout bounds how long a client may take to send request headers. It is
// deliberately not configurable and deliberately much shorter than cfg.ReadTimeout:
// it is the slowloris guard, and a per-request read budget generous enough to be
// useful to real handlers is far too generous to also serve as one.
const readHeaderTimeout = 5 * time.Second

// Deps is everything the server needs from the outside. Only Logger has a fallback
// (slog.Default), so the zero value starts a server that refuses every request — which
// is what lets the tests run one with no database, no blob volume and no upstream.
type Deps struct {
	Verifier *auth.Verifier
	Handlers *api.Handlers
	Ready    func(ctx context.Context) error
	Version  string
	Logger   *slog.Logger
}

// Server owns the two listeners and the metrics registry behind them. Build it with
// New and run it with Run.
type Server struct {
	cfg     config.Config
	deps    Deps
	log     *slog.Logger
	metrics *metrics
	ready   atomic.Bool

	started     chan struct{}
	mu          sync.Mutex
	publicAddr  net.Addr
	metricsAddr net.Addr
}

// New returns a Server with its metrics registry already built and populated. The
// ready flag starts false, so /readyz answers 503 until the caller has finished its
// own start-up checks and calls SetReady(true).
func New(cfg config.Config, deps Deps) *Server {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		cfg:     cfg,
		deps:    deps,
		log:     log,
		metrics: newMetrics(deps.Version),
		started: make(chan struct{}),
	}
	// Wire the pack byte counter into the handler without the api package having to
	// know about the registry. New is called once, before any request is served, so
	// the assignment is not a data race.
	if deps.Handlers != nil {
		deps.Handlers.PackUpload = func(bytes int64) {
			s.metrics.packUploadBytes.Add(float64(bytes))
		}
		deps.Handlers.Published = func(channel string) {
			s.metrics.publishTotal.WithLabelValues(channel).Inc()
		}
		deps.Handlers.ValidationFailed = func() {
			s.metrics.validationFails.Inc()
		}
	}
	return s
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

// MetricsAddr returns the address the internal listener is bound to, with the same
// port-0 caveat as PublicAddr.
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
		return fmt.Errorf("listen on CONFIG_LISTEN_ADDR %q: %w", s.cfg.ListenAddr, err)
	}
	metricsLn, err := net.Listen("tcp", s.cfg.MetricsAddr)
	if err != nil {
		_ = publicLn.Close()
		return fmt.Errorf("listen on CONFIG_METRICS_ADDR %q: %w", s.cfg.MetricsAddr, err)
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

	s.mu.Lock()
	s.publicAddr = publicLn.Addr()
	s.metricsAddr = metricsLn.Addr()
	s.mu.Unlock()
	close(s.started)

	g, groupCtx := errgroup.WithContext(ctx)

	g.Go(func() error {
		<-groupCtx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ShutdownTimeout)
		defer cancel()

		s.log.Info("shutting down", slog.Duration("timeout", s.cfg.ShutdownTimeout))

		// Both listeners shut down against one shared deadline, so neither waits out
		// the other's in-flight requests.
		errs := make(chan error, 2)
		go func() { errs <- publicSrv.Shutdown(shutdownCtx) }()
		go func() { errs <- metricsSrv.Shutdown(shutdownCtx) }()
		return errors.Join(<-errs, <-errs)
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

// publicHandler assembles the public mux and its middleware chain.
//
// Middleware is wrapped innermost-first, so a request passes through request ID, then
// the access log, then metrics, then panic recovery, then the route's token check, and
// finally the router. The order is load-bearing: request ID has to be outermost so the
// recovery handler can put the ID in a 500 body, and recovery has to be inside the log
// and metrics so a panicking request is still counted and logged with its real status.
func (s *Server) publicHandler() http.Handler {
	routes := api.Routes()

	mux := http.NewServeMux()
	for _, rt := range routes {
		mux.Handle(rt.Pattern(), s.guard(rt, s.deps.Handlers.For(rt)))
	}

	// The fallback is guarded as well, as the least privileged role. §5 has no public
	// route: everything under /api/admin/config is staff-only, so an unauthenticated
	// request for a path that does not exist is answered exactly like one for a path
	// that does, and the service cannot be used to enumerate its own route table.
	mux.Handle("/", s.guard(
		api.Route{Method: "*", Path: "/", MinRole: auth.RoleViewer},
		api.NotFoundOrMethodNotAllowed(routes),
	))

	var h http.Handler = mux
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
// set, and then the injected dependency check must pass. It is called once per /readyz
// request, so the work behind deps.Ready must be cheap; main composes it from a cached
// Postgres ping, a stat of the blob root, and the verifier's key count for exactly
// that reason.
func (s *Server) readiness(ctx context.Context) error {
	if !s.ready.Load() {
		return errors.New("service is still starting up")
	}
	if s.deps.Ready == nil {
		return nil
	}
	return s.deps.Ready(ctx)
}
