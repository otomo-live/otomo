// Command config is the Otomo configuration and content service: it owns namespaces,
// their drafts and versions, content packs, release channels and the audit trail, and
// it serves the published manifests that clients and other services read.
//
// One binary, three commands. serve runs the HTTP service; migrate applies the embedded
// goose migrations; seed creates the starter namespaces the binary carries. Migrations
// are a separate one-shot step rather than startup work
// because several replicas starting at once would each try to migrate, and because a
// schema change should be a deliberate, observable act with its own exit code rather
// than a side effect of a restart.
//
// This service holds no signing key. It verifies staff tokens against PHP Admin Auth's
// JWKS and never mints one, so a compromised Config container cannot issue tokens.
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
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/otomo-live/otomo/services/config/internal/api"
	"github.com/otomo-live/otomo/services/config/internal/auth"
	"github.com/otomo-live/otomo/services/config/internal/blob"
	"github.com/otomo-live/otomo/services/config/internal/config"
	"github.com/otomo-live/otomo/services/config/internal/schema"
	"github.com/otomo-live/otomo/services/config/internal/seed"
	"github.com/otomo-live/otomo/services/config/internal/server"
	"github.com/otomo-live/otomo/services/config/internal/store"
	"github.com/otomo-live/otomo/services/config/migrations"
)

// version is the build identifier reported in the startup log line and as
// config_build_info on /metrics. The Docker build stamps it with
// -X main.version=${GIT_SHA}; a plain `go build` leaves the "dev" default.
var version = "dev"

const (
	dbPingTimeout = 5 * time.Second

	usage = `usage: config [command]

commands:
  serve    run the HTTP service (default)
  migrate  apply the embedded database migrations
  seed     create the embedded starter namespaces (skip existing ones)
           --dry-run reports what would be created without writing
`
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run dispatches the first argument to a subcommand and returns its process exit
// code. No arguments means serve, which is the default. Anything unrecognised prints
// the usage text and returns 2.
func run(args []string) int {
	if len(args) == 0 {
		return runServe(nil)
	}

	switch args[0] {
	case "serve":
		return runServe(args[1:])
	case "migrate":
		return runMigrate(args[1:])
	case "seed":
		return runSeed(args[1:])
	default:
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
}

// runServe runs the HTTP service until the process is signalled, then shuts both
// listeners down within cfg.ShutdownTimeout.
//
// Startup is fail-fast for everything this process can check locally and patient about
// everything it cannot. Postgres and the blob volume are checked before the readiness
// flag is set, so a misconfigured container dies immediately with a specific message
// rather than serving errors. PHP Admin Auth is the exception: an unavailable JWKS is
// not a reason to refuse to start, because the endpoint may simply not be deployed
// yet — the service starts, rejects every token until keys arrive, and reports itself
// unready in the meantime. That is the honest description of a service that cannot
// verify anyone, and it keeps a Config deploy from being coupled to an Auth deploy.
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
	log.Info("config starting",
		slog.String("version", version),
		slog.String("listen_addr", cfg.ListenAddr),
		slog.String("metrics_addr", cfg.MetricsAddr),
		slog.String("blob_root", cfg.BlobRoot),
		slog.String("staff_issuer", cfg.StaffIssuer),
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	bootCtx, cancelBoot := context.WithTimeout(ctx, dbPingTimeout)
	defer cancelBoot()

	db, err := store.NewPool(bootCtx, cfg)
	if err != nil {
		log.Error("postgres unusable at startup", slog.Any("error", err))
		return 1
	}
	defer db.Close()

	blobs, err := blob.NewStore(cfg.BlobRoot)
	if err != nil {
		log.Error("blob volume unusable at startup", slog.Any("error", err))
		return 1
	}

	// Reclaim temp files a previous process could not clean up after itself. This is
	// housekeeping, not a correctness requirement: a file left in tmp/ is never a blob
	// and nothing reads it. So a failure here is logged and start-up continues —
	// refusing to serve because old temporary files cannot be deleted would trade a
	// slow leak for an outage.
	if removed, err := blobs.SweepTemps(blob.TempMaxAge); err != nil {
		log.Warn("could not sweep stale upload temp files",
			slog.Int("removed", removed), slog.Any("error", err))
	} else if removed > 0 {
		log.Info("swept stale upload temp files", slog.Int("removed", removed))
	}

	// Start is deliberately before the verifier gates readiness: it returns
	// immediately and fetches in the background, so the boot timeout above only ever
	// covers Postgres.
	verifier := auth.NewVerifier(cfg.StaffJWKSURL, cfg.StaffIssuer, cfg.StaffAudience, cfg.JWKSRefresh, cfg.JWTClockSkew)
	verifier.Start(ctx)

	cancelBoot()

	srv := server.New(cfg, server.Deps{
		Verifier: verifier,
		Handlers: &api.Handlers{Store: db, Log: log, Schemas: &schema.Cache{}, Blobs: blobs,
			MaxPackBytes: cfg.MaxPackBytes},
		Ready:   readyCheck(db, blobs, verifier),
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

// readyCheck composes the three things /readyz must be honest about: Postgres
// reachable, the blob directory still present, and at least one staff key fetched.
//
// All three are cached or cheap by construction — the database probe is rate-limited
// inside store.DB, the blob check is a single stat, and the key check reads an
// in-memory map — because a load balancer may call this every second. The three are
// joined rather than short-circuited so the response names every reason at once; an
// operator fixing one problem should not have to discover the next by waiting for
// another poll.
func readyCheck(db *store.DB, blobs *blob.Store, verifier *auth.Verifier) func(context.Context) error {
	return func(ctx context.Context) error {
		var errs []error
		if err := db.Ready(ctx); err != nil {
			errs = append(errs, err)
		}
		if err := blobs.Ready(); err != nil {
			errs = append(errs, err)
		}
		// Not ready until PHP Admin Auth's JWKS has been fetched once. This service
		// cannot verify a single staff token until then, so reporting ready would let
		// a load balancer send it traffic it can only answer with 401s.
		if !verifier.Ready(ctx) {
			errs = append(errs, fmt.Errorf("staff jwks not fetched yet from %s", verifier.URL()))
		}
		return errors.Join(errs...)
	}
}

// runMigrate applies the embedded goose migrations and returns.
//
// It opens its own database/sql handle rather than the pgx pool, because goose wants
// a *sql.DB and this runs as a one-shot command, never alongside serve. Applying the
// same migrations twice is a no-op: goose tracks applied versions in its own table, so
// the Jenkins migration job can run before every deploy without needing to know
// whether it already did (CFG-A2).
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

// runSeed creates the starter namespaces embedded in the binary and returns.
//
// It is a one-shot command like migrate, so it loads the same database-only
// configuration and never starts a listener. Seeding is idempotent by rule: a namespace
// that already exists is skipped rather than overwritten, which is what makes this safe
// to run against a live database. --dry-run answers "what would change?" by loading the
// seed folder and probing Postgres without writing.
//
// One JSON log line per namespace records the outcome; any error is a process error, so
// the exit code is the whole story for Jenkins.
func runSeed(args []string) int {
	flags := flag.NewFlagSet("seed", flag.ContinueOnError)
	dryRun := flags.Bool("dry-run", false, "report what would be created without writing")
	if code := parse(flags, args); code >= 0 {
		return code
	}

	cfg, err := config.Load(config.ModeMigrate)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	log := newLogger(cfg.LogLevel)

	// Load before opening the database: a malformed seed folder is a build problem and
	// should fail without touching Postgres at all.
	namespaces, err := seed.Load(seed.FS)
	if err != nil {
		log.Error("seed folder is invalid", slog.Any("error", err))
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	bootCtx, cancelBoot := context.WithTimeout(ctx, dbPingTimeout)
	defer cancelBoot()

	db, err := store.NewPool(bootCtx, cfg)
	if err != nil {
		log.Error("postgres unusable", slog.Any("error", err))
		return 1
	}
	defer db.Close()
	cancelBoot()

	var report seed.Report
	if *dryRun {
		report, err = seed.Plan(ctx, db, namespaces)
	} else {
		report, err = seed.Apply(ctx, db, namespaces, seed.Actor)
	}
	if err != nil {
		log.Error("seed failed", slog.Any("error", err))
		return 1
	}

	for _, result := range report.Results {
		log.Info("seed namespace",
			slog.String("namespace", result.Name),
			slog.String("action", string(result.Action)),
			slog.Bool("dry_run", *dryRun),
		)
	}
	return 0
}

// parse parses a subcommand's flags. It returns -1 when the caller should carry on,
// or an exit code to return immediately: 0 for an explicit -h/-help, 2 for a
// malformed flag or a stray non-flag argument.
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

// newLogger returns the process-wide JSON logger: one JSON object per line on stdout
// at the configured level. Logs go to stdout rather than stderr so the container log
// stream is a single ordered sequence.
func newLogger(level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}
