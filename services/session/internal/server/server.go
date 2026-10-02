// Package server builds and runs the service's three HTTP listeners.
//
// The public listener carries the player API and the small staff admin surface, and
// requires a token from the route's own identity domain on every route, including the
// fallback. The metrics listener carries /healthz, /readyz, /metrics and pprof, and is
// never routed to from outside the Docker network; it has no authentication of its own,
// so that isolation is the only thing keeping it private. The internal listener (LB-4)
// carries the Allocator's callback, needs session_allocator.key on every path, and no
// gateway points at it. All three start together and shut down together against a
// single deadline, so a SIGTERM cannot leave one draining while another has already
// closed.
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

	"github.com/otomo-live/otomo/services/session/internal/api"
	"github.com/otomo-live/otomo/services/session/internal/auth"
	"github.com/otomo-live/otomo/services/session/internal/config"
	"github.com/otomo-live/otomo/services/session/internal/servicekey"
)

// readHeaderTimeout bounds how long a client may take to send request headers. It is
// deliberately not configurable and deliberately much shorter than cfg.ReadTimeout: it
// is the slowloris guard, and a per-request read budget generous enough to hold an event
// long-poll open is far too generous to also serve as one.
const readHeaderTimeout = 5 * time.Second

// Deps is everything the server needs from the outside. Only Logger has a fallback
// (slog.Default), so the zero value starts a server that refuses every request — which is
// what lets the tests run one with no database and no Valkey.
//
// The two verifiers are separate fields rather than one field with a domain beside it,
// because that is the shape of the boundary: a route names a group, the guard picks the
// verifier for it, and a server missing one of the two refuses the routes of that group
// instead of quietly accepting tokens it cannot check.
type Deps struct {
	PlayerVerifier *auth.Verifier
	StaffVerifier  *auth.Verifier
	// Handlers is the code behind the routes. Nil leaves every route at its 501
	// placeholder, which is what the guard tests rely on.
	Handlers *api.Handlers
	Ready    func(ctx context.Context) error
	Version  string
	Logger   *slog.Logger
	// Collectors are extra metrics to export on /metrics, such as the rules loader's.
	Collectors []prometheus.Collector
	// CallbackKeys are the D4 keys the internal listener accepts (session_allocator.key).
	// Nil accepts nobody: every internal route answers 401.
	CallbackKeys *servicekey.Keys
	// Release is the live channel head the release check compares X-Otomo-Release with
	// (SE-8). Nil leaves every player request unchecked, except that a missing header
	// is still refused when SESSION_REQUIRE_RELEASE_HEADER is set.
	Release ReleaseHead
}

// Server owns the two listeners and the metrics registry behind them. Build it with New
// and run it with Run.
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

// New returns a Server with its metrics registry already built and populated. The ready
// flag starts false, so /readyz answers 503 until the caller has finished its own
// start-up checks and calls SetReady(true).
func New(cfg config.Config, deps Deps) *Server {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	m := newMetrics(deps.Version)
	m.registry.MustRegister(deps.Collectors...)
	return &Server{
		cfg:     cfg,
		deps:    deps,
		log:     log,
		metrics: m,
		started: make(chan struct{}),
	}
}

// SetReady flips the flag /readyz consults. Call it once every start-up dependency has
// been verified; never call it before Run, or the service will report ready while nothing
// is listening.
func (s *Server) SetReady(ready bool) {
	s.ready.Store(ready)
}

// Started returns a channel that is closed once both listeners are bound and their
// addresses are recorded. Tests wait on it instead of polling or sleeping, which is what
// keeps them from being flaky on a slow machine.
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

// MetricsAddr returns the address the metrics listener is bound to, with the same port-0
// caveat as PublicAddr.
func (s *Server) MetricsAddr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.metricsAddr
}

// InternalAddr returns the address the internal listener (SESSION_INTERNAL_ADDR) is
// bound to, with the same port-0 caveat as PublicAddr.
func (s *Server) InternalAddr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.internalAddr
}

// Run binds both listeners and serves until ctx is cancelled, then shuts them down
// gracefully within cfg.ShutdownTimeout and returns.
//
// Binding happens before the goroutines start, so a port already in use is reported as a
// plain error rather than a listener that dies asynchronously. Cancelling ctx stops
// accepting new connections but lets in-flight requests finish, which is what makes a
// rolling deploy drop no requests — and here it also lets a held long-poll answer on its
// own terms, since the handler watches the request context rather than the server's.
func (s *Server) Run(ctx context.Context) error {
	publicLn, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on SESSION_LISTEN_ADDR %q: %w", s.cfg.ListenAddr, err)
	}
	metricsLn, err := net.Listen("tcp", s.cfg.MetricsAddr)
	if err != nil {
		_ = publicLn.Close()
		return fmt.Errorf("listen on SESSION_METRICS_ADDR %q: %w", s.cfg.MetricsAddr, err)
	}
	internalLn, err := net.Listen("tcp", s.cfg.InternalAddr)
	if err != nil {
		_ = publicLn.Close()
		_ = metricsLn.Close()
		return fmt.Errorf("listen on SESSION_INTERNAL_ADDR %q: %w", s.cfg.InternalAddr, err)
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
		Handler:           s.metricsHandler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       s.cfg.ReadTimeout,
		WriteTimeout:      s.cfg.WriteTimeout,
		IdleTimeout:       s.cfg.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	internalSrv := &http.Server{
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
	s.internalAddr = internalLn.Addr()
	s.mu.Unlock()
	close(s.started)

	g, groupCtx := errgroup.WithContext(ctx)

	g.Go(func() error {
		<-groupCtx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ShutdownTimeout)
		defer cancel()

		s.log.Info("shutting down", slog.Duration("timeout", s.cfg.ShutdownTimeout))

		// All three listeners shut down against one shared deadline, so none waits out
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

// publicHandler assembles the public mux and its middleware chain.
//
// Middleware is wrapped innermost-first, so a request passes through request ID, then the
// access log, then metrics, then panic recovery, then the route's token check, and
// finally the router. The order is load-bearing: request ID has to be outermost so the
// recovery handler can put the ID in a 500 body, and recovery has to be inside the log
// and metrics so a panicking request is still counted and logged with its real status.
func (s *Server) publicHandler() http.Handler {
	routes := api.Routes()

	mux := http.NewServeMux()
	for _, rt := range routes {
		h := s.deps.Handlers.For(rt)
		if rt.Group == api.GroupPlayer {
			h = s.releaseCheck(h)
		}
		mux.Handle(rt.Pattern(), s.guard(rt, h))
	}

	// The fallback is guarded too, with the *player* domain — see below. There is no
	// unauthenticated route in §5, so an unauthenticated request for a path that does not
	// exist is answered exactly like one for a path that does, and the service cannot be
	// used to enumerate its own route table.
	//
	// Which domain guards the fallback is a real choice rather than a detail: it decides
	// which 401 a scanner gets. The player domain is picked because it is the one whose
	// issuer every game client holds a token for, so a client that mistypes a path sees
	// the same rejection it would get for a real route. A staff token on this path is
	// refused, which is correct — nothing under /api/player/session is a staff route.
	// NotFoundOrMethodNotAllowed still names the allowed methods of a real path, and it
	// reports them the same way whatever token was presented, so a 405 on
	// /api/admin/session/players leaks only that the path exists.
	mux.Handle("/", s.guard(
		api.Route{Method: "*", Path: "/", Group: api.GroupPlayer, MinRole: auth.RoleNone},
		api.NotFoundOrMethodNotAllowed(routes),
	))

	var h http.Handler = mux
	h = s.withRecover(h)
	h = s.metrics.middleware(h)
	h = s.withAccessLog(h)
	h = withRequestID(h)
	return h
}

// internalHandler assembles the internal listener's mux: the Allocator's callback
// (design/14-launch-handoff.md §4.4), with the same request ID, access log, metrics and
// panic recovery as the public listener.
//
// The key check wraps the whole mux, fallback included, so a caller without
// session_allocator.key gets 401 on every path and cannot learn which ones exist. With
// the key, an unknown path is 404.
func (s *Server) internalHandler() http.Handler {
	mux := http.NewServeMux()
	if s.deps.Handlers != nil {
		mux.HandleFunc(api.AllocationEndedPattern, s.deps.Handlers.AllocationEnded)
	} else {
		mux.HandleFunc(api.AllocationEndedPattern, api.NotImplemented)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		api.WriteError(w, r, http.StatusNotFound, "not_found", "no such route")
	})

	var h http.Handler = s.deps.CallbackKeys.Require(servicekey.RoleAllocator, mux)
	h = s.withRecover(h)
	h = s.metrics.middleware(h)
	h = s.withAccessLog(h)
	h = withRequestID(h)
	return h
}

// metricsHandler assembles the metrics listener's mux: health, readiness, Prometheus
// metrics and pprof, with no authentication and no access logging.
//
// Nothing here is routed through Gateway, and the port is not published in the container
// image. That isolation is the whole access control story: pprof and /metrics both leak
// more than they should if this listener is ever exposed.
func (s *Server) metricsHandler() http.Handler {
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

// readiness reports whether the service can serve traffic: the startup flag must be set,
// and then the injected dependency check must pass. It is called once per /readyz
// request, so the work behind deps.Ready must be cheap; main composes it from a cached
// Postgres ping, a Valkey ping and each verifier's key count for exactly that reason.
func (s *Server) readiness(ctx context.Context) error {
	if !s.ready.Load() {
		return errors.New("service is still starting up")
	}
	if s.deps.Ready == nil {
		return nil
	}
	return s.deps.Ready(ctx)
}
