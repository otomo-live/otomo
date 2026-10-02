// Command session is the Otomo session service: it owns the social state of a player
// outside a match — their profile, presence, friends, blocks and party — and delivers
// the events that change it to clients.
//
// One binary, two commands. serve runs the HTTP service; migrate applies the embedded
// goose migrations. Migrations are a separate one-shot step rather than startup work
// because several replicas starting at once would each try to migrate, and because a
// schema change should be a deliberate, observable act with its own exit code rather
// than a side effect of a restart.
//
// This service holds no signing key. It verifies player tokens against Auth's JWKS and
// staff tokens against PHP Admin Auth's, and mints neither, so a compromised Session
// container cannot issue a token for any domain.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/otomo-live/otomo/services/session/internal/allocator"
	"github.com/otomo-live/otomo/services/session/internal/api"
	"github.com/otomo-live/otomo/services/session/internal/auth"
	"github.com/otomo-live/otomo/services/session/internal/config"
	"github.com/otomo-live/otomo/services/session/internal/events"
	"github.com/otomo-live/otomo/services/session/internal/launch"
	"github.com/otomo-live/otomo/services/session/internal/presence"
	"github.com/otomo-live/otomo/services/session/internal/ratelimit"
	"github.com/otomo-live/otomo/services/session/internal/rules"
	"github.com/otomo-live/otomo/services/session/internal/server"
	"github.com/otomo-live/otomo/services/session/internal/servicekey"
	"github.com/otomo-live/otomo/services/session/internal/store"
	"github.com/otomo-live/otomo/services/session/migrations"
)

// version is the build identifier reported in the startup log line and as
// session_build_info on /metrics. The Docker build stamps it with
// -X main.version=${GIT_SHA}; a plain `go build` leaves the "dev" default.
var version = "dev"

const (
	// bootTimeout bounds the two start-up probes that can hang on a network problem —
	// the Postgres ping and the Valkey ping. Neither is given its own timeout: they are
	// both "is the dependency there", and a container that fails either should die with
	// one message rather than two.
	bootTimeout = 5 * time.Second

	usage = `usage: session [command]

commands:
  serve    run the HTTP service (default)
  migrate  apply the embedded database migrations
`
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run dispatches the first argument to a subcommand and returns its process exit code.
// No arguments means serve, which is the default. Anything unrecognised prints the usage
// text and returns 2.
func run(args []string) int {
	if len(args) == 0 {
		return runServe(nil)
	}

	switch args[0] {
	case "serve":
		return runServe(args[1:])
	case "migrate":
		return runMigrate(args[1:])
	default:
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
}

// runServe runs the HTTP service until the process is signalled, then shuts all three
// listeners down within cfg.ShutdownTimeout.
//
// Startup is fail-fast for everything this process can check locally and patient about
// everything it cannot. Postgres and Valkey are checked before the readiness flag is set,
// so a misconfigured container dies immediately with a specific message rather than
// serving errors: Postgres holds the durable state, and Valkey holds the event stream
// without which no client would ever be told that anything changed.
//
// The two issuers are the exception. An unavailable JWKS is not a reason to refuse to
// start, because the endpoint may simply not be deployed yet — the service starts,
// rejects every token on that domain until keys arrive, and reports itself unready in the
// meantime. That keeps a Session deploy from being coupled to a deploy of Auth or of PHP
// Admin Auth.
func runServe(args []string) int {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	if code := parse(flags, args); code >= 0 {
		return code
	}

	cfg, err := config.Load(config.ModeServe)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	log := newLogger(cfg.LogLevel)
	log.Info("session starting",
		slog.String("version", version),
		slog.String("listen_addr", cfg.ListenAddr),
		slog.String("metrics_addr", cfg.MetricsAddr),
		slog.String("internal_addr", cfg.InternalAddr),
		slog.String("player_issuer", cfg.PlayerIssuer),
		slog.String("staff_issuer", cfg.StaffIssuer),
		slog.Duration("event_hold", cfg.EventHold),
		slog.String("patch_url", cfg.PatchURL),
		slog.String("rules_channel", cfg.RulesChannel),
		slog.Bool("require_release_header", cfg.RequireReleaseHeader),
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	bootCtx, cancelBoot := context.WithTimeout(ctx, bootTimeout)
	defer cancelBoot()

	db, err := store.NewPool(bootCtx, cfg)
	if err != nil {
		log.Error("postgres unusable at startup", slog.Any("error", err))
		return 1
	}
	defer db.Close()

	evts, err := events.New(bootCtx, cfg.ValkeyURL)
	if err != nil {
		log.Error("valkey unusable at startup", slog.Any("error", err))
		return 1
	}
	defer evts.Close()

	// One pub/sub subscription for the whole instance, fanned out to the held polls
	// (SE-4). When ctx ends it closes the hub, so held polls answer at once and the
	// server's shutdown does not wait out a 25 s hold.
	hub := events.NewHub()
	go evts.Subscribe(ctx, hub, log)

	// SES-D5: expired invites cannot be accepted anyway; this only stops them piling up.
	go sweepInvites(ctx, db, log)

	// SE-3: presence shares the event stream's Valkey connection. Every instance trims
	// the online set every 15 s; the trim script hands each player who went offline to
	// exactly one instance.
	pres := presence.NewService(presence.New(evts.Conn()), db, evts, log)
	go pres.Run(ctx)

	// Start is deliberately before the verifiers gate readiness: it returns immediately
	// and fetches in the background, so the boot timeout above only ever covers the two
	// pings.
	playerVerifier := auth.NewPlayerVerifier(
		cfg.PlayerJWKSURL, cfg.PlayerIssuer, cfg.PlayerAudience, cfg.JWKSRefresh, cfg.JWTClockSkew)
	playerVerifier.Start(ctx)
	staffVerifier := auth.NewStaffVerifier(
		cfg.StaffJWKSURL, cfg.StaffIssuer, cfg.StaffAudience, cfg.JWKSRefresh, cfg.JWTClockSkew)
	staffVerifier.Start(ctx)

	cancelBoot()

	// LB-1: session.rules from the live server manifest on Patch's internal listener,
	// every SESSION_RULES_POLL. It serves the compiled-in defaults until its first good
	// document, and keeps the last good rules through any failure, so it never gates
	// readiness. Without a key it stays on the defaults.
	var (
		ruleSource  rules.Source = rules.Static{R: rules.Defaults()}
		releaseHead server.ReleaseHead
	)
	collectors := pres.Collectors()
	if cfg.PatchKeyPath != "" {
		key, err := readKeyFile(cfg.PatchKeyPath)
		if err != nil {
			log.Error("patch service key unusable at startup", slog.Any("error", err))
			return 1
		}
		loader := rules.NewLoader(rules.LoaderConfig{
			PatchURL: cfg.PatchURL,
			Channel:  cfg.RulesChannel,
			Key:      key,
			Interval: cfg.RulesPoll,
			Log:      log,
		})
		go loader.Run(ctx)
		ruleSource, releaseHead = loader, loader
		collectors = append(collectors, loader.Collectors()...)
	} else {
		log.Warn("SESSION_PATCH_KEY_PATH is not set; session.rules stay at the compiled-in defaults")
	}

	// LB-3: launches ask the Allocator for a game server after launching is committed,
	// and the sweep repeats any launch a restart cut short. Without a key, launching
	// answers 503 launch_unavailable.
	var (
		launcher api.Launcher
		tickets  api.TicketIssuer
		status   launch.StatusReader
	)
	if cfg.AllocatorKeyPath != "" {
		key, err := readKeyFile(cfg.AllocatorKeyPath)
		if err != nil {
			log.Error("allocator service key unusable at startup", slog.Any("error", err))
			return 1
		}
		alloc := allocator.New(cfg.AllocatorURL, key, nil)
		l := launch.New(ctx, db, alloc, evts, log)
		go l.Run(ctx)
		launcher, tickets, status = l, alloc, alloc
		collectors = append(collectors, l.Collectors()...)
	} else {
		log.Warn("SESSION_ALLOCATOR_KEY_PATH is not set; lobby launches answer 503 launch_unavailable")
	}

	// LB-4: a match's end returns its party to forming, told by the Allocator's
	// callback on the internal listener or found by the repair poll. The poll needs the
	// Allocator key; the callback needs session_allocator.key, and without it every call
	// to the internal listener answers 401.
	returner := launch.NewReturner(ctx, db, status, evts, log)
	if status != nil {
		go returner.Run(ctx)
	}
	collectors = append(collectors, returner.Collectors()...)
	var callbackKeys *servicekey.Keys
	if cfg.CallbackKeyPath != "" {
		callbackKeys, err = servicekey.LoadKeys(map[servicekey.Role]string{servicekey.RoleAllocator: cfg.CallbackKeyPath})
		if err != nil {
			log.Error("allocator callback key unusable at startup", slog.Any("error", err))
			return 1
		}
	} else {
		log.Warn("SESSION_CALLBACK_KEY_PATH is not set; the Allocator's callback answers 401 and only the repair poll returns parties from a match")
	}

	srv := server.New(cfg, server.Deps{
		PlayerVerifier: playerVerifier,
		StaffVerifier:  staffVerifier,
		Collectors:     collectors,
		CallbackKeys:   callbackKeys,
		Release:        releaseHead,
		Handlers: &api.Handlers{
			Profiles:  db,
			Log:       log,
			Rules:     ruleSource,
			Events:    evts,
			Hub:       hub,
			EventHold: cfg.EventHold,
			Parties:   db,
			Publisher: evts,
			Launcher:  launcher,
			Tickets:   tickets,
			Returns:   returner,
			Presence:  pres,
			Friends:   db,
			Admin:     db,
			Limits:    ratelimit.New(evts.Conn()),
		},
		Ready:   readyCheck(db, evts, playerVerifier, staffVerifier),
		Version: version,
		Logger:  log,
	})
	srv.SetReady(true)

	if err := srv.Run(ctx); err != nil {
		log.Error("listener stopped with an error", slog.Any("error", err))
		return 1
	}
	log.Info("shutdown complete")
	return 0
}

// readyCheck composes the four things /readyz must be honest about: Postgres reachable,
// Valkey reachable, and at least one key fetched from each of the two identity domains.
//
// All four are cheap by construction — the database probe is rate-limited inside
// store.DB, the Valkey probe is one PING on a multiplexed connection, and the key checks
// read in-memory maps — because a load balancer may call this every second. They are
// joined rather than short-circuited so the response names every reason at once; an
// operator fixing one problem should not have to discover the next by waiting for another
// poll.
//
// Both domains are required, and neither is optional on the grounds that "only some
// routes need it": a Session that cannot verify players cannot serve the game at all, and
// one that cannot verify staff cannot be administered. Reporting ready while either is
// dark would let a load balancer send traffic this instance can only answer with 401s.
func readyCheck(db *store.DB, evts *events.Client, player, staff *auth.Verifier) func(context.Context) error {
	return func(ctx context.Context) error {
		var errs []error
		if err := db.Ready(ctx); err != nil {
			errs = append(errs, err)
		}
		if err := evts.Ping(ctx); err != nil {
			errs = append(errs, err)
		}
		if !player.Ready(ctx) {
			errs = append(errs, fmt.Errorf("player jwks not fetched yet from %s", player.URL()))
		}
		if !staff.Ready(ctx) {
			errs = append(errs, fmt.Errorf("staff jwks not fetched yet from %s", staff.URL()))
		}
		return errors.Join(errs...)
	}
}

// readKeyFile reads a D4 service key file and returns its text, ready to send as a
// bearer token. The key itself never appears in an error.
func readKeyFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read key file %q: %w", path, err)
	}
	key := strings.TrimSpace(string(raw))
	if len(key) < 43 { // 32 bytes of base64url
		return "", fmt.Errorf("key file %q is too short to be a generated service key", path)
	}
	return key, nil
}

// inviteSweepInterval is how often expired party invites are deleted (SES-D5).
const inviteSweepInterval = time.Minute

// sweepInvites deletes expired party invites every inviteSweepInterval until ctx ends.
// Every instance runs it; the DELETE is idempotent, so running it twice costs one
// statement.
func sweepInvites(ctx context.Context, db *store.DB, log *slog.Logger) {
	t := time.NewTicker(inviteSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := db.DeleteExpiredInvites(ctx); err != nil && ctx.Err() == nil {
				log.Warn("expired invite sweep failed", slog.Any("error", err))
			}
		}
	}
}

// runMigrate applies the embedded goose migrations and returns.
//
// It opens its own database/sql handle rather than the pgx pool, because goose wants a
// *sql.DB and this runs as a one-shot command, never alongside serve. Applying the same
// migrations twice is a no-op: goose tracks applied versions in its own table, so the
// Jenkins migration job can run before every deploy without needing to know whether it
// already did (SES-A2).
func runMigrate(args []string) int {
	flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
	if code := parse(flags, args); code >= 0 {
		return code
	}

	cfg, err := config.Load(config.ModeMigrate)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	log := newLogger(cfg.LogLevel)

	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		log.Error("cannot open postgres", slog.Any("error", err))
		return 1
	}
	defer db.Close()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		log.Error("cannot set the goose dialect", slog.Any("error", err))
		return 1
	}
	if err := goose.Up(db, "."); err != nil {
		log.Error("migration failed", slog.Any("error", err))
		return 1
	}

	log.Info("migrations applied")
	return 0
}

// parse parses a subcommand's flags. It returns -1 when the caller should carry on, or an
// exit code to return immediately: 0 for an explicit -h/-help, 2 for a malformed flag or
// a stray non-flag argument.
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

// newLogger returns the process-wide JSON logger: one JSON object per line on stdout at
// the configured level. Logs go to stdout rather than stderr so the container log stream
// is a single ordered sequence.
func newLogger(level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}
