// Command dashboard is the Otomo live-ops dashboard: a thin, access-controlled query layer
// in front of Prometheus and Loki. It owns no data of its own — no database, no index — and
// its whole job is to let staff see service health, logs and the audit trail without giving
// the browser a path to the observability stores.
//
// One binary, one command. serve runs the HTTP service. There is no migrate: the Dashboard
// has no schema to migrate.
//
// This service holds no signing key. It verifies staff tokens against PHP Admin Auth's JWKS
// and never mints one, so a compromised Dashboard container cannot issue tokens.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/otomo-live/otomo/services/dashboard/internal/api"
	"github.com/otomo-live/otomo/services/dashboard/internal/auditsrc"
	"github.com/otomo-live/otomo/services/dashboard/internal/auth"
	"github.com/otomo-live/otomo/services/dashboard/internal/config"
	"github.com/otomo-live/otomo/services/dashboard/internal/health"
	"github.com/otomo-live/otomo/services/dashboard/internal/loki"
	"github.com/otomo-live/otomo/services/dashboard/internal/prometheus"
	"github.com/otomo-live/otomo/services/dashboard/internal/server"
)

// version is the build identifier reported in the startup log line and as
// dashboard_build_info on /metrics. The Docker build stamps it with
// -X main.version=${GIT_SHA}; a plain `go build` leaves the "dev" default.
var version = "dev"

const usage = `usage: dashboard [command]

commands:
  serve    run the HTTP service (default)
`

func main() {
	os.Exit(run(os.Args[1:]))
}

// run dispatches the first argument to a subcommand and returns its process exit code. No
// arguments means serve, which is the default. Anything unrecognised prints the usage text
// and returns 2.
func run(args []string) int {
	if len(args) == 0 {
		return runServe(nil)
	}

	switch args[0] {
	case "serve":
		return runServe(args[1:])
	default:
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
}

// runServe runs the HTTP service until the process is signalled, then shuts both listeners
// down within cfg.ShutdownTimeout.
//
// Startup is fail-fast for everything this process can check locally. PHP Admin Auth is the
// exception: an unavailable JWKS is not a reason to refuse to start, because the endpoint
// may simply not be deployed yet — the service starts, rejects every token until keys
// arrive, and reports itself unready in the meantime. The same patience extends to
// Prometheus and Loki, which are not checked at all at boot: they are query-time
// dependencies, and an upstream that is down must degrade a panel, not the process.
func runServe(args []string) int {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	if code := parse(flags, args); code >= 0 {
		return code
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	log := newLogger(cfg.LogLevel)
	log.Info("dashboard starting",
		slog.String("version", version),
		slog.String("listen_addr", cfg.ListenAddr),
		slog.String("metrics_addr", cfg.MetricsAddr),
		slog.String("staff_issuer", cfg.StaffIssuer),
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Start is deliberately before readiness is believable: it returns immediately and
	// fetches in the background, so boot never blocks on PHP Admin Auth.
	verifier := auth.NewVerifier(cfg.StaffJWKSURL, cfg.StaffIssuer, cfg.StaffAudience, cfg.JWKSRefresh, cfg.JWTClockSkew)
	verifier.Start(ctx)

	// The prober owns no data and needs no key: it polls each configured service's
	// /readyz on its own goroutine and feeds the snapshot to /services and to the
	// dashboard_service_* metrics. It runs on the same ctx as both listeners so a
	// shutdown stops the polling with the serving.
	prober := health.NewProber(healthTargets(cfg.Targets), cfg.ProbeInterval, cfg.UpstreamTimeout, nil)

	// The metrics source is query-time only: it is built here but never called at
	// start-up, so an unreachable Prometheus degrades a panel rather than the process.
	// It implements source.MetricsSource and backs both the overview batch and the
	// per-service series handler.
	metrics := prometheus.New(cfg.PrometheusURL, nil, cfg.UpstreamTimeout)

	// The Loki client backs log search and the live tail. Like the metrics source it is
	// built here and only called at query time, so an unreachable Loki degrades the log
	// panel rather than the process.
	logs := loki.New(cfg.LokiURL, nil, cfg.UpstreamTimeout)

	// The three audit feeds behind GET /audit. Each forwards the caller's own bearer token
	// and is only called at query time; a feed that is down degrades the merged page
	// rather than the process.
	audit := []*auditsrc.Client{
		auditsrc.New("config", cfg.ConfigURL, "/api/admin/config/audit", nil, cfg.UpstreamTimeout),
		auditsrc.New("admin-auth", cfg.AdminAuthURL, "/admin-auth/audit", nil, cfg.UpstreamTimeout),
		auditsrc.New("session", cfg.SessionURL, "/api/admin/session/audit", nil, cfg.UpstreamTimeout),
	}

	handlers := &api.Handlers{
		Services:    prober.Snapshot,
		Metrics:     metrics,
		AllowList:   metrics.AllowList,
		Logs:        logs,
		LogServices: logs.Services,
		Audit:       audit,
	}

	srv := server.New(cfg, server.Deps{
		Verifier: verifier,
		Handlers: handlers,
		Ready:    readyCheck(verifier),
		Version:  version,
		Logger:   log,
		Metrics:  metrics,
	})
	// The server is the single implementation of every instrumentation seam: the prober's
	// Observer, the handlers' cache/tail/degraded events, and the three upstream clients.
	// Wiring happens here, once, before any listener or prober starts.
	prober.Observer = srv
	metrics.Observer = srv
	logs.Observer = srv
	handlers.Observer = srv
	for _, c := range audit {
		c.Observer = srv
	}
	srv.SetReady(true)

	go prober.Run(ctx)
	if len(cfg.Targets) > 0 {
		log.Info("service prober started",
			slog.Int("targets", len(cfg.Targets)),
			slog.Duration("interval", cfg.ProbeInterval),
		)
	}

	if err := srv.Run(ctx); err != nil {
		log.Error("listener stopped with an error", slog.Any("error", err))
		return 1
	}
	log.Info("shutdown complete")
	return 0
}

// readyCheck is the one thing /readyz is honest about: at least one staff key has been
// fetched, so the service can verify a token. A key check is an in-memory map read, because
// a load balancer may call this every second.
//
// Prometheus and Loki reachability is deliberately not part of it. A down metrics or log
// store must degrade the panels that depend on it — an empty chart, an error on the log pane
// (DSH-D8) — not take the Dashboard out of rotation. Reporting unready because Prometheus is
// briefly down would turn a degraded view into no view at all, which is worse for the person
// trying to find out what is broken.
func readyCheck(verifier *auth.Verifier) func(context.Context) error {
	return func(ctx context.Context) error {
		if !verifier.Ready(ctx) {
			return fmt.Errorf("staff jwks not fetched yet from %s", verifier.URL())
		}
		return nil
	}
}

// parse parses a subcommand's flags. It returns -1 when the caller should carry on, or an
// exit code to return immediately: 0 for an explicit -h/-help, 2 for a malformed flag or a
// stray non-flag argument.
func parse(flags *flag.FlagSet, args []string) int {
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument %q\n\n%s", flags.Arg(0), usage)
		return 2
	}
	return -1
}

// healthTargets converts the configured name/url pairs into the prober's own target type.
// The two are kept separate so config does not depend on the probing machinery and the
// prober can be exercised without an environment.
func healthTargets(targets []config.Target) []health.Target {
	out := make([]health.Target, len(targets))
	for i, t := range targets {
		out[i] = health.Target{Name: t.Name, URL: t.URL}
	}
	return out
}

// newLogger returns the process-wide JSON logger: one JSON object per line on stdout at the
// configured level. Logs go to stdout rather than stderr so the container log stream is a
// single ordered sequence.
func newLogger(level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}
