package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"gateway/gateway/internal/apierr"
	"gateway/gateway/internal/authn"
	"gateway/gateway/internal/config"
	"gateway/gateway/internal/obslog"
	"gateway/gateway/internal/proxy"
	"gateway/gateway/internal/ratelimit"
	"gateway/gateway/internal/router"
	"gateway/gateway/internal/server"
)

var version = "dev"

func main() {
	obslog.Init()

	cfg, err := config.Load()
	if err != nil {
		slog.Error("configuration error", "err", err)
		os.Exit(1)
	}

	slog.Info("gateway starting",
		"version", version,
		"listen", cfg.ListenAddr,
		"metrics", cfg.MetricsAddr,
	)

	// No switch: this binary serves the player routes, and the admin and dev
	// edge is services/gateway_dev.
	routes := router.BuildRoutes()

	reg := proxy.NewRegistry(cfg.Upstreams)
	if err := verifyUpstreams(routes, reg); err != nil {
		slog.Error("route/upstream mismatch", "err", err)
		os.Exit(1)
	}

	// main owns the signal handling: the same cancellation ends the listeners,
	// the JWKS clients' background refresh and the rate limiters' sweepers.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	publicMux, jwks, err := buildPublicMux(ctx, cfg, routes, reg)
	if err != nil {
		slog.Error("route/issuer mismatch", "err", err)
		os.Exit(1)
	}

	if err := server.Run(ctx, cfg, jwks.Ready, publicMux); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}

// loginPatterns lists the route patterns that get the stricter login rate
// limit. Techspec §6.4: "/auth/*" is the brute-force target on this service
// ("/admin-auth/*" is services/gateway_dev). A guard test asserts every entry
// is a real route, so a rename cannot silently drop the stricter limit.
var loginPatterns = map[string]bool{
	"/auth/": true,
}

// buildPublicMux builds everything behind the public listener from the config,
// route table and proxy registry: the JWKS registry (started on ctx), the two
// per-IP rate limiters (sweepers stopped by ctx) and the route mux. main uses
// it, and so do the tests, so both exercise the same wiring.
func buildPublicMux(ctx context.Context, cfg *config.Config, routes []router.Route, reg *proxy.Registry) (*http.ServeMux, *authn.Registry, error) {
	// One JWKS client per authentication domain this route table actually uses:
	// the player domain, plus the staff domain for the two dev/staging manifest
	// routes. Both are in the table, so /readyz waits on both.
	jwksURLs, issuers := jwksSources(cfg, router.UsedGroups(routes))
	if err := verifyIssuers(routes, issuers, jwksURLs); err != nil {
		return nil, nil, err
	}

	jwks := authn.NewRegistry(jwksURLs)
	jwks.Start(ctx)

	generalRL := ratelimit.New(cfg.RateLimitRPS, cfg.RateLimitBurst)
	generalRL.StartSweeper(ctx, cfg.SweepInterval, cfg.MaxIdleAge)
	loginRL := ratelimit.New(cfg.LoginRateLimitRPS, cfg.LoginRateLimitBurst)
	loginRL.StartSweeper(ctx, cfg.SweepInterval, cfg.MaxIdleAge)

	publicMux := http.NewServeMux()
	registerRoutes(publicMux, routes, reg, authn.RequireGroup(jwks, issuers, cfg.JWTClockSkew), generalRL, loginRL)
	return publicMux, jwks, nil
}

// jwksSources maps each authentication domain a route table uses onto its key
// source and expected identity.
//
// This lives in main because it is the one place that already knows both the
// route table and the config, and neither of those should have to know the
// other: `router` stays a pure data package, and `config` stays a list of env
// vars rather than learning what a "player domain" is.
func jwksSources(cfg *config.Config, used map[router.Group]bool) (map[router.Group]string, map[router.Group]authn.Issuer) {
	urls := make(map[router.Group]string, len(used))
	issuers := make(map[router.Group]authn.Issuer, len(used))
	for group := range used {
		switch group {
		case router.GroupPlayer:
			urls[group] = cfg.PlayerJWKSURL
			issuers[group] = authn.Issuer{Issuer: cfg.PlayerIssuer, Audience: cfg.PlayerAudience}
		case router.GroupStaff:
			urls[group] = cfg.StaffJWKSURL
			issuers[group] = authn.Issuer{Issuer: cfg.StaffIssuer, Audience: cfg.StaffAudience}
		}
	}
	return urls, issuers
}

// verifyUpstreams asserts every route's Upstream key resolves to a proxy in
// the registry. Exit-worthy: an unknown upstream becomes a nil deref on the
// first matching request.
func verifyUpstreams(routes []router.Route, reg *proxy.Registry) error {
	for _, route := range routes {
		if !reg.Has(route.Upstream) {
			return fmt.Errorf("route %s %s: upstream %q not in registry",
				route.Method, route.Pattern, route.Upstream)
		}
	}
	return nil
}

// verifyIssuers asserts every protected route's domain has both a JWKS URL and
// an expected issuer/audience. Exit-worthy for the same reason verifyUpstreams
// is: a route whose domain has no key source would otherwise be registered with
// a middleware that has nothing to verify against. The middleware fails closed
// if it happens anyway, but a process that starts and 500s every request is
// harder to diagnose than one that refuses to start.
func verifyIssuers(routes []router.Route, issuers map[router.Group]authn.Issuer, urls map[router.Group]string) error {
	for _, route := range routes {
		if route.Group == router.GroupPublic {
			continue
		}
		if urls[route.Group] == "" {
			return fmt.Errorf("route %s: group %s has no JWKS URL configured",
				route.MuxPattern(), route.Group)
		}
		iss := issuers[route.Group]
		if iss.Issuer == "" || iss.Audience == "" {
			return fmt.Errorf("route %s: group %s has no expected issuer/audience configured",
				route.MuxPattern(), route.Group)
		}
	}
	return nil
}

// registerRoutes adds every route to the mux with the handler chain, plus a
// catch-all 404 in the COM-5 shape.
//
// Request-time order (outermost first): withGroup -> rateLimit -> auth -> proxy.
// withGroup is outermost so the group is already on the request info by the
// time the limiter or auth rejects, which is what labels a rejection's metric
// and log line with the domain it was rejected for. The limiter runs before
// auth so a flood of bad tokens is turned away before any signature check.
func registerRoutes(mux *http.ServeMux, routes []router.Route, reg *proxy.Registry, auth authn.Middleware, generalRL, loginRL *ratelimit.Limiter) {
	for _, route := range routes {
		handler := auth(route, reg.HandlerFor(route))
		rl := generalRL
		if loginPatterns[route.Pattern] {
			rl = loginRL
		}
		handler = rl.Wrap(handler)
		handler = withGroup(route.Group, handler)
		mux.Handle(route.MuxPattern(), handler)
	}

	// Catch-all: ServeMux has no NotFoundHandler, so "/" at lowest specificity
	// ensures unmatched paths return COM-5 JSON instead of plain text.
	mux.Handle("/", withGroup(router.GroupPublic, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apierr.WriteError(w, r, http.StatusNotFound, "not_found", "the requested path does not exist")
	})))
}

func withGroup(g router.Group, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		obslog.SetGroup(r.Context(), g.String())
		next.ServeHTTP(w, r)
	})
}
