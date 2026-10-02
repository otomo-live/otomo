// Package server builds and runs the Allocator's two HTTP listeners.
//
// The Allocator is internal-only: it has no public route and no gateway identity. Game
// servers register and heartbeat with it, and Session asks it for a game server for a
// party; both go through the API listener and authenticate with a service key (design
// decision D4). This ticket is the skeleton, so no route is registered yet — Deps.Routes
// is the seam the registry and allocation tickets fill.
//
// The metrics listener carries /healthz, /readyz, /metrics and pprof, and is never
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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"

	"github.com/otomo-live/otomo/services/allocator/internal/api"
	"github.com/otomo-live/otomo/services/allocator/internal/config"
)

// readHeaderTimeout bounds how long a client may take to send request headers. It is
// deliberately not configurable and deliberately much shorter than cfg.ReadTimeout:
// it is the slowloris guard, and a per-request read budget generous enough to be
// useful to real handlers is far too generous to also serve as one.
const readHeaderTimeout = 5 * time.Second

// Deps is everything the server needs from the outside. Only Logger has a fallback
// (slog.Default), so the zero value starts a server that reports itself unready —
// which is what lets the tests run one with no database.
//
// Ready is the live dependency check behind /readyz. main composes it from store.DB.Ready
// so the endpoint reports an unreachable Postgres honestly; it is optional so a
// zero-value Deps still builds a server.
//
// Routes is where later tickets register the registry and allocation handlers. It is
// called with the API mux before the catch-all is added, so a nil Routes leaves every
// API path answering the COM-5 404. A Routes implementation must not register "/" —
// that is the catch-all.
type Deps struct {
	Ready   func(ctx context.Context) error
	Routes  func(mux *http.ServeMux)
	Version string
	Logger  *slog.Logger
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
	apiAddr     net.Addr
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

// APIAddr returns the address the API listener is bound to. It is the configured
// address except when that asked for port 0, which is how a test gets a free port; the
// result is only meaningful once Started is closed.
func (s *Server) APIAddr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.apiAddr
}

// MetricsAddr returns the address the metrics listener is bound to, with the same
// port-0 caveat as APIAddr.
func (s *Server) MetricsAddr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.metricsAddr
}

// Registry returns the Prometheus registry the /metrics endpoint serves, so later
// tickets can register their own collectors on the same instance rather than reaching
// for the process-wide default registry.
func (s *Server) Registry() *prometheus.Registry {
	return s.metrics.registry
}

// Run binds both listeners and serves until ctx is cancelled, then shuts them down
// gracefully within cfg.ShutdownTimeout and returns.
//
// Binding happens before the goroutines start, so a port already in use is reported as
// a plain error rather than a listener that dies asynchronously. Cancelling ctx stops
// accepting new connections but lets in-flight requests finish, which is what makes a
// rolling deploy drop no requests.
func (s *Server) Run(ctx context.Context) error {
	apiLn, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on ALLOCATOR_LISTEN_ADDR %q: %w", s.cfg.ListenAddr, err)
	}
	metricsLn, err := net.Listen("tcp", s.cfg.MetricsAddr)
	if err != nil {
		_ = apiLn.Close()
		return fmt.Errorf("listen on ALLOCATOR_METRICS_ADDR %q: %w", s.cfg.MetricsAddr, err)
	}

	apiSrv := &http.Server{
		Handler:           s.apiHandler(),
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
	s.apiAddr = apiLn.Addr()
	s.metricsAddr = metricsLn.Addr()
	s.mu.Unlock()
	close(s.started)

	g, groupCtx := errgroup.WithContext(ctx)

	g.Go(func() error {
		<-groupCtx.Done()

		// WithoutCancel keeps the shutdown context's values but detaches it from the
		// cancellation that started the shutdown, so the servers get the full
		// ShutdownTimeout to drain rather than an already-expired context.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ShutdownTimeout)
		defer cancel()

		s.log.Info("shutting down", slog.Duration("timeout", s.cfg.ShutdownTimeout))

		// Both listeners shut down against one shared deadline, so neither waits out
		// the other's in-flight requests.
		errs := make(chan error, 2)
		go func() { errs <- apiSrv.Shutdown(shutdownCtx) }()
		go func() { errs <- metricsSrv.Shutdown(shutdownCtx) }()
		return errors.Join(<-errs, <-errs)
	})
	g.Go(func() error {
		if err := apiSrv.Serve(apiLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("api listener: %w", err)
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

// apiHandler assembles the API mux and its middleware chain.
//
// Middleware is wrapped innermost-first, so a request passes through request ID, then
// the access log, then metrics, then panic recovery, and finally the router. The order
// matters: request ID is outermost so every later layer and the error envelope can see
// it, and recovery is innermost so the failed request is still counted and logged with
// its real 500.
//
// Deps.Routes runs before the catch-all is registered, so a server assembled without
// routes answers every API path through the COM-5 catch-all. The catch-all is
// unauthenticated on purpose: a 401 for an unknown path would leak the shape of the
// route table, and route registration is the ticket that adds the auth wrapper.
func (s *Server) apiHandler() http.Handler {
	mux := http.NewServeMux()
	if s.deps.Routes != nil {
		s.deps.Routes(mux)
	}
	mux.Handle("/", http.HandlerFunc(api.NotFound))

	var h http.Handler = mux
	h = s.withRecover(h)
	h = s.metrics.middleware(h)
	h = s.withAccessLog(h)
	h = withRequestID(h)
	return h
}

// internalHandler assembles the metrics mux: health, readiness, Prometheus metrics
// and pprof, with no authentication and no access logging.
//
// Nothing here is routed from outside the Docker network, and the port is not
// published in the container image. That isolation is the whole access control story —
// pprof and /metrics both leak more than they should if this listener is ever exposed.
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
// cached Postgres ping for exactly that reason.
//
// The check is optional so the zero-value Deps produces a server that reports itself
// unready rather than panicking.
func (s *Server) readiness(ctx context.Context) error {
	if !s.ready.Load() {
		return errors.New("service is still starting up")
	}
	if s.deps.Ready != nil {
		if err := s.deps.Ready(ctx); err != nil {
			return err
		}
	}
	return nil
}
