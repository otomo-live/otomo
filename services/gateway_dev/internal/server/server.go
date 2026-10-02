package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"net/netip"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"

	"github.com/otomo-live/otomo/services/gateway_dev/internal/clientip"
	"github.com/otomo-live/otomo/services/gateway_dev/internal/config"
	"github.com/otomo-live/otomo/services/gateway_dev/internal/obslog"
)

const shutdownTimeout = 15 * time.Second

// ReadinessFunc reports whether the process is ready to serve. It is called on
// every readiness probe, so implementations must not block.
type ReadinessFunc func(context.Context) bool

// Run starts the public and metrics listeners and blocks until shutdown
// completes or a fatal error occurs. It returns nil when ctx is cancelled.
//
// ctx is the caller's: main owns the signal handling, so the same cancellation
// that ends the listeners also ends the JWKS clients' background refresh.
func Run(ctx context.Context, cfg *config.Config, ready ReadinessFunc, publicMux *http.ServeMux) error {
	publicHandler := BuildPublicHandler(publicMux, cfg.TrustedProxies)
	metricsMux := BuildMetricsMux(ready)

	publicServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           publicHandler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}

	metricsServer := &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      0, // pprof CPU profiles default to 30s; a finite timeout cuts them
		IdleTimeout:       cfg.IdleTimeout,
	}

	g, gCtx := errgroup.WithContext(ctx)

	g.Go(func() error {
		slog.Info("public listener starting", "addr", cfg.ListenAddr, "tls", cfg.TLSCertFile != "")
		var err error
		if cfg.TLSCertFile != "" {
			err = publicServer.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
		} else {
			err = publicServer.ListenAndServe()
		}
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	})

	g.Go(func() error {
		slog.Info("metrics listener starting", "addr", cfg.MetricsAddr)
		err := metricsServer.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	})

	g.Go(func() error {
		<-gCtx.Done()
		slog.Info("shutting down", "timeout", shutdownTimeout)

		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		pubErr := publicServer.Shutdown(shutdownCtx)
		metErr := metricsServer.Shutdown(shutdownCtx)
		return errors.Join(pubErr, metErr)
	})

	return g.Wait()
}

// BuildPublicHandler wraps the public mux with the middleware chain:
// clientip → requestID → logging → recover → mux.
// ClientIP is outermost so the resolved address is what requestID, logging,
// recover and every inner layer see. RequestID is next so the ID is on the
// context for every downstream consumer. Logging follows so it emits an access
// log line even on panic. Recover is innermost so it catches panics, writes
// COM-5 500 through the status-capturing writer, and returns normally — Logging
// still runs.
func BuildPublicHandler(mux *http.ServeMux, trustedProxies []netip.Prefix) http.Handler {
	var h http.Handler = mux
	h = obslog.RecoverMiddleware(h)
	h = obslog.LoggingMiddleware(h)
	h = obslog.RequestIDMiddleware(h)
	h = clientip.Middleware(trustedProxies, h)
	return h
}

// BuildMetricsMux creates the internal mux carrying /healthz, /readyz,
// /metrics, and pprof. These endpoints must be unreachable on GATEWAY_LISTEN_ADDR.
func BuildMetricsMux(ready ReadinessFunc) *http.ServeMux {
	mux := http.NewServeMux()

	// /healthz is liveness only: the process is up. It says nothing about
	// whether traffic can be served, which is why it does not consult ready.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// /readyz is 503 until every authentication domain has fetched its keys.
	// A nil check means "no readiness source configured" — which only happens
	// in tests that build the mux without a gateway around it.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if ready == nil || ready(r.Context()) {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	mux.Handle("GET /metrics", promhttp.Handler())

	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	return mux
}
