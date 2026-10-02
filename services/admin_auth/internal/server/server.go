// Package server builds and runs the service's two HTTP listeners.
//
// The public listener carries the API. The internal listener carries /healthz,
// /readyz, /metrics and pprof, and is never routed to from outside the Docker
// network — it has no authentication of its own, so that isolation is the only thing
// keeping it private. Both listeners start together and shut down together against a
// single deadline, so a SIGTERM cannot leave one draining while the other has already
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

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"

	"github.com/otomo-live/otomo/services/admin_auth/internal/api"
	"github.com/otomo-live/otomo/services/admin_auth/internal/config"
	"github.com/otomo-live/otomo/services/admin_auth/internal/token"
)

// readHeaderTimeout bounds how long a client may take to send request headers. It is
// deliberately not configurable and deliberately much shorter than cfg.ReadTimeout:
// it is the slowloris guard, and a per-request read budget generous enough to be
// useful to real handlers is far too generous to also serve as one.
const readHeaderTimeout = 5 * time.Second

// Deps is everything the server needs from the outside. Only Logger has a fallback
// (slog.Default), and every field is optional, so the zero value starts a usable
// server — which is what lets the tests run one with no dependencies at all. No field
// may be nil-checked at call time; the constructors here are the only checking.
type Deps struct {
	JWKS         api.JWKSProvider
	Signer       *token.Signer
	Verifier     *token.Verifier
	Store        api.Store
	MFAKey       []byte
	MFANow       func() time.Time
	Verify       func(hash, password string) (bool, error)
	Ready        func(ctx context.Context) error
	Keys         func() error
	Version      string
	JWKSKeyCount int
	Logger       *slog.Logger
}

// Server owns the two listeners and the metrics registry behind them. Build it with
// New and run it with Run; the addresses are only meaningful between Started closing
// and Run returning.
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
// own startup checks and calls SetReady(true).
func New(cfg config.Config, deps Deps) *Server {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		cfg:     cfg,
		deps:    deps,
		log:     log,
		metrics: newMetrics(deps.Version, deps.JWKSKeyCount),
		started: make(chan struct{}),
	}
}

// SetReady flips the flag /readyz consults. Call it once every startup dependency
// has been verified; never call it before Run, or the service will report ready
// while nothing is listening.
func (s *Server) SetReady(ready bool) {
	s.ready.Store(ready)
}

// Started returns a channel that is closed once both listeners are bound and their
// addresses are recorded. Tests wait on it instead of polling or sleeping, which is
// what keeps them from being flaky on a slow machine.
func (s *Server) Started() <-chan struct{} {
	return s.started
}

// PublicAddr returns the address the public listener is bound to. It is the
// configured address except when that asked for port 0, which is how a test gets a
// free port; the result is only meaningful once Started is closed.
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
// Binding happens before the goroutines start, so a port already in use is reported
// as a plain error rather than a listener that dies asynchronously. Cancelling ctx
// stops accepting new connections but lets in-flight requests finish, which is what
// makes a rolling deploy drop no requests.
func (s *Server) Run(ctx context.Context) error {
	publicLn, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on ADMIN_AUTH_LISTEN_ADDR %q: %w", s.cfg.ListenAddr, err)
	}
	metricsLn, err := net.Listen("tcp", s.cfg.MetricsAddr)
	if err != nil {
		_ = publicLn.Close()
		return fmt.Errorf("listen on ADMIN_AUTH_METRICS_ADDR %q: %w", s.cfg.MetricsAddr, err)
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

		// Both listeners shut down against one shared deadline, so neither
		// waits out the other's in-flight requests.
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
// Middleware is wrapped innermost-first, so a request passes through request ID,
// then the access log, then metrics, then panic recovery, and finally the router.
// The order is load-bearing: request ID has to be outermost so the recovery handler
// can put the ID in a 500 body, and recovery has to be inside the log and metrics so
// a panicking request is still counted and logged with its real status.
//
// The implemented public routes are the JWKS, staff login, refresh, logout and /me;
// the rest of the staff business endpoints arrive with AA-4 and later. Every other
// path falls through to the COM-5 404, including internal-only paths such as
// /healthz, which must look absent from the outside.
func (s *Server) publicHandler() http.Handler {
	routes := []api.Route{
		{Method: http.MethodGet, Path: "/.well-known/jwks.json"},
		{Method: http.MethodPost, Path: "/admin-auth/login"},
		{Method: http.MethodPost, Path: "/admin-auth/mfa/verify"},
		{Method: http.MethodPost, Path: "/admin-auth/mfa/enroll"},
		{Method: http.MethodPost, Path: "/admin-auth/mfa/confirm"},
		{Method: http.MethodPost, Path: "/admin-auth/onboard/lookup"},
		{Method: http.MethodPost, Path: "/admin-auth/onboard"},
		{Method: http.MethodPost, Path: "/admin-auth/refresh"},
		{Method: http.MethodPost, Path: "/admin-auth/logout"},
		{Method: http.MethodGet, Path: "/admin-auth/me"},
		{Method: http.MethodPost, Path: "/admin-auth/account/password"},
		{Method: http.MethodGet, Path: "/admin-auth/account/sessions"},
		{Method: http.MethodPost, Path: "/admin-auth/account/sessions/revoke-others"},
		{Method: http.MethodPost, Path: "/admin-auth/account/mfa/recovery-codes"},
		{Method: http.MethodPost, Path: "/admin-auth/account/mfa/disable"},
		{Method: http.MethodGet, Path: "/admin-auth/audit"},
		{Method: http.MethodGet, Path: "/api/admin/users"},
		{Method: http.MethodPost, Path: "/api/admin/users"},
		{Method: http.MethodGet, Path: "/api/admin/users/invites"},
		{Method: http.MethodDelete, Path: "/api/admin/users/invites/{id}"},
		{Method: http.MethodPatch, Path: "/api/admin/users/{id}"},
		{Method: http.MethodPost, Path: "/api/admin/users/{id}/reset"},
		{Method: http.MethodPost, Path: "/api/admin/users/{id}/mfa/reset"},
	}

	mux := http.NewServeMux()
	mux.Handle("GET /.well-known/jwks.json", api.JWKS(s.deps.JWKS))
	mux.Handle("POST /admin-auth/login", api.Login(api.LoginDeps{
		Store:       s.deps.Store,
		Signer:      s.deps.Signer,
		AccessTTL:   s.cfg.AccessTokenTTL,
		RefreshTTL:  s.cfg.RefreshTokenTTL,
		MaxFailures: s.cfg.LoginMaxFailures,
		Lockout:     s.cfg.LoginLockout,
		Logger:      s.log,
		Verify:      s.deps.Verify,
		OnResult:    func(result string) { s.metrics.staffLogin.WithLabelValues(result).Inc() },
	}))
	mux.Handle("POST /admin-auth/mfa/verify", api.MFAVerify(api.MFADeps{
		Store:      s.deps.Store,
		Signer:     s.deps.Signer,
		AccessTTL:  s.cfg.AccessTokenTTL,
		RefreshTTL: s.cfg.RefreshTokenTTL,
		Key:        s.deps.MFAKey,
		Logger:     s.log,
		OnResult:   func(result string) { s.metrics.staffMfa.WithLabelValues(result).Inc() },
		Now:        s.deps.MFANow,
	}))
	mux.Handle("POST /admin-auth/mfa/enroll", api.MFAEnroll(api.MFADeps{
		Store:    s.deps.Store,
		Verifier: s.deps.Verifier,
		Key:      s.deps.MFAKey,
		Logger:   s.log,
		OnResult: func(result string) { s.metrics.staffMfa.WithLabelValues(result).Inc() },
	}))
	mux.Handle("POST /admin-auth/mfa/confirm", api.MFAConfirm(api.MFADeps{
		Store:      s.deps.Store,
		Signer:     s.deps.Signer,
		Verifier:   s.deps.Verifier,
		AccessTTL:  s.cfg.AccessTokenTTL,
		RefreshTTL: s.cfg.RefreshTokenTTL,
		Key:        s.deps.MFAKey,
		Logger:     s.log,
		OnResult:   func(result string) { s.metrics.staffMfa.WithLabelValues(result).Inc() },
		Now:        s.deps.MFANow,
	}))
	mux.Handle("POST /admin-auth/onboard/lookup", api.OnboardLookup(api.OnboardDeps{
		Store:      s.deps.Store,
		Signer:     s.deps.Signer,
		AccessTTL:  s.cfg.AccessTokenTTL,
		RefreshTTL: s.cfg.RefreshTokenTTL,
		Logger:     s.log,
	}))
	mux.Handle("POST /admin-auth/onboard", api.Onboard(api.OnboardDeps{
		Store:      s.deps.Store,
		Signer:     s.deps.Signer,
		AccessTTL:  s.cfg.AccessTokenTTL,
		RefreshTTL: s.cfg.RefreshTokenTTL,
		Logger:     s.log,
		OnResult:   func(result string) { s.metrics.staffLogin.WithLabelValues(result).Inc() },
	}))
	mux.Handle("POST /admin-auth/refresh", api.Refresh(api.RefreshDeps{
		Store:      s.deps.Store,
		Signer:     s.deps.Signer,
		AccessTTL:  s.cfg.AccessTokenTTL,
		RefreshTTL: s.cfg.RefreshTokenTTL,
		Grace:      s.cfg.RefreshReuseGrace,
		Logger:     s.log,
		OnResult:   func(result string) { s.metrics.staffRefresh.WithLabelValues(result).Inc() },
	}))
	mux.Handle("POST /admin-auth/logout", api.Logout(api.LogoutDeps{
		Store:  s.deps.Store,
		Logger: s.log,
	}))
	mux.Handle("GET /admin-auth/me", api.Me(api.MeDeps{
		Store:    s.deps.Store,
		Verifier: s.deps.Verifier,
		Logger:   s.log,
	}))
	accountDeps := api.AccountDeps{
		Store:       s.deps.Store,
		Verifier:    s.deps.Verifier,
		Key:         s.deps.MFAKey,
		MaxFailures: s.cfg.LoginMaxFailures,
		Lockout:     s.cfg.LoginLockout,
		Logger:      s.log,
		Now:         s.deps.MFANow,
	}
	mux.Handle("POST /admin-auth/account/password", api.AccountPassword(accountDeps))
	mux.Handle("GET /admin-auth/account/sessions", api.AccountSessions(accountDeps))
	mux.Handle("POST /admin-auth/account/sessions/revoke-others", api.AccountRevokeOtherSessions(accountDeps))
	mux.Handle("POST /admin-auth/account/mfa/recovery-codes", api.AccountRegenerateRecoveryCodes(accountDeps))
	mux.Handle("POST /admin-auth/account/mfa/disable", api.AccountDisableMFA(accountDeps))
	mux.Handle("GET /admin-auth/audit", s.requireRole("viewer")(api.Audit(api.AuditDeps{
		Store:  s.deps.Store,
		Logger: s.log,
	})))

	// The user-management API. gateway_dev forwards /api/admin/users* unstripped and
	// already requires the admin role; requireAdmin repeats both the token check and
	// the database role check as defence in depth.
	adminDeps := api.AdminDeps{
		Store:     s.deps.Store,
		InviteTTL: s.cfg.InviteTTL,
		PublicURL: s.cfg.PublicURL,
		Logger:    s.log,
	}
	mux.Handle("GET /api/admin/users", s.requireAdmin(api.ListAdminUsers(adminDeps)))
	mux.Handle("POST /api/admin/users", s.requireAdmin(api.InviteUser(adminDeps)))
	mux.Handle("GET /api/admin/users/invites", s.requireAdmin(api.ListInvites(adminDeps)))
	mux.Handle("DELETE /api/admin/users/invites/{id}", s.requireAdmin(api.RevokeInvite(adminDeps)))
	mux.Handle("PATCH /api/admin/users/{id}", s.requireAdmin(api.PatchUser(adminDeps)))
	mux.Handle("POST /api/admin/users/{id}/reset", s.requireAdmin(api.ResetUserPassword(adminDeps)))
	mux.Handle("POST /api/admin/users/{id}/mfa/reset", s.requireAdmin(api.ResetUserMFA(adminDeps)))

	mux.Handle("/", api.NotFoundOrMethodNotAllowed(routes))

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
// set, then the injected dependency checks must pass. It is called once per /readyz
// request, so the work behind deps.Ready must be cached; store.DB.Ready caches its
// probe for exactly this reason. deps.Keys is expected to be cheap too.
//
// Both checks matter, and the order is deliberate: a database that is down is the
// more fundamental failure, so it is reported before the signing-key check. deps.Keys
// is the real check — main supplies a closure that fails while the JWKS cache holds no
// key — so a service that somehow started without publishing a key is kept out of
// rotation instead of signing tokens no verifier can resolve.
func (s *Server) readiness(ctx context.Context) error {
	if !s.ready.Load() {
		return errors.New("service is still starting up")
	}
	if s.deps.Ready != nil {
		if err := s.deps.Ready(ctx); err != nil {
			return err
		}
	}
	if s.deps.Keys != nil {
		if err := s.deps.Keys(); err != nil {
			return err
		}
	}
	return nil
}
