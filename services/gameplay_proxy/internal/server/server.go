// Package server owns the proxy's private HTTP listener: health, readiness and
// Prometheus metrics.
//
// The proxy's real listener is UDP, so unlike the Allocator this service has no HTTP
// API and no authentication. The one HTTP port it opens carries only /healthz,
// /readyz and /metrics, and it is never routed to from outside the Docker network;
// that isolation is the whole access-control story, because /metrics leaks operational
// detail and has no credentials of its own.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/otomo-live/otomo/services/gameplay_proxy/internal/config"
)

// readHeaderTimeout bounds how long a client may take to send request headers. It is
// deliberately not configurable: it is the slowloris guard, and it is much shorter than
// any read budget a real handler would want.
const readHeaderTimeout = 5 * time.Second

// Deps is everything the server needs from outside. Check is the live readiness check
// behind /readyz; it is optional so a zero-value Deps still builds a server that
// reports itself unready.
type Deps struct {
	Ready   func(ctx context.Context) error
	Version string
	Logger  *slog.Logger
}

// Server owns the HTTP listener and the metrics registry behind it. Build it with New
// and run it with Run.
type Server struct {
	cfg     config.Config
	deps    Deps
	log     *slog.Logger
	metrics *serverMetrics
	ready   atomic.Bool

	started chan struct{}
	mu      sync.Mutex
	addr    net.Addr
}

// New returns a Server with its metrics registry already built. The ready flag starts
// false, so /readyz answers 503 until the caller has finished its own start-up checks
// and calls SetReady(true).
func New(cfg config.Config, deps Deps) *Server {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		cfg:     cfg,
		deps:    deps,
		log:     log,
		metrics: newServerMetrics(deps.Version),
		started: make(chan struct{}),
	}
}

// SetReady flips the flag /readyz consults. Call it once every start-up dependency has
// loaded; readiness is derived from Deps.Ready, so there is no separate latch here.
func (s *Server) SetReady(ready bool) {
	s.ready.Store(ready)
}

// Registry returns the Prometheus registry /metrics serves, so the domain instruments
// register on the same instance rather than the process-wide default.
func (s *Server) Registry() *prometheus.Registry {
	return s.metrics.registry
}

// Started is closed once the listener is bound.
func (s *Server) Started() <-chan struct{} {
	return s.started
}

// Addr returns the bound HTTP address, or nil before Run. It is the configured address
// except when that asked for port 0.
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Run binds the listener and serves until ctx is cancelled, then shuts down within
// cfg.ShutdownTimeout. Binding happens before the goroutine starts, so a port already
// in use is a plain error.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.MetricsAddr)
	if err != nil {
		return fmt.Errorf("listen on PROXY_METRICS_ADDR %q: %w", s.cfg.MetricsAddr, err)
	}

	srv := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}

	s.mu.Lock()
	s.addr = ln.Addr()
	s.mu.Unlock()
	close(s.started)

	serveErr := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	// WithoutCancel keeps the shutdown context's values but detaches it from the
	// cancellation that started the shutdown, so the server gets the full timeout to
	// drain rather than an already-expired context.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ShutdownTimeout)
	defer cancel()
	s.log.Info("shutting down", slog.Duration("timeout", s.cfg.ShutdownTimeout))
	return srv.Shutdown(shutdownCtx)
}

// handler assembles the private mux. /healthz is liveness only and touches no
// dependency, so a failed Allocator cannot get the container killed; /readyz reports
// whether the JWKS and directory have loaded.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !s.ready.Load() {
			http.Error(w, "service is still starting up", http.StatusServiceUnavailable)
			return
		}
		if s.deps.Ready != nil {
			if err := s.deps.Ready(r.Context()); err != nil {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.metrics.registry, promhttp.HandlerOpts{}))
	return mux
}

// serverMetrics is the private listener's own registry and the build-info gauge. The
// domain instruments (gameplay_proxy_*) are registered onto the same registry by
// internal/metrics.
type serverMetrics struct {
	registry  *prometheus.Registry
	buildInfo *prometheus.GaugeVec
}

// newServerMetrics builds a fresh registry rather than using the global default, so two
// servers in one test binary do not collide. version is published as
// gameplay_proxy_build_info; it changes only on restart, which a deploy is.
func newServerMetrics(version string) *serverMetrics {
	reg := prometheus.NewRegistry()
	m := &serverMetrics{
		registry: reg,
		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gameplay_proxy_build_info",
			Help: "Build information; the sample value is always 1.",
		}, []string{"version"}),
	}
	m.buildInfo.WithLabelValues(version).Set(1)

	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.buildInfo,
	)
	return m
}
