// Command patch is the Otomo patch service: it serves the current release manifest per
// channel and the content-addressed blobs those manifests name.
//
// Patch is deliberately small and read-only. It holds no authoring logic — Config owns
// that — and it connects to Postgres as the read-only role patch_ro, so a compromised
// Patch container cannot change a release. This ticket is only the skeleton: the
// manifest loader (PAT-B2), the NOTIFY listener (PAT-B3/B4), the manifest endpoint
// (PAT-B5) and blob delivery (PAT-A2) arrive later. There is one command, serve, and no
// migrate command, because Patch owns no schema.
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
	"time"

	"github.com/otomo-live/otomo/services/patch/internal/auth"
	"github.com/otomo-live/otomo/services/patch/internal/blob"
	"github.com/otomo-live/otomo/services/patch/internal/config"
	"github.com/otomo-live/otomo/services/patch/internal/manifest"
	"github.com/otomo-live/otomo/services/patch/internal/server"
	"github.com/otomo-live/otomo/services/patch/internal/servicekey"
	"github.com/otomo-live/otomo/services/patch/internal/store"
)

// version is the build identifier reported in the startup log line and as
// patch_build_info on /metrics. The Docker build stamps it with
// -X main.version=${GIT_SHA}; a plain `go build` leaves the "dev" default.
var version = "dev"

const (
	dbPingTimeout       = 5 * time.Second
	manifestLoadTimeout = 10 * time.Second

	usage = `usage: patch [command]

commands:
  serve    run the HTTP service (default)
`
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run dispatches the first argument to the serve command and returns its process exit
// code. No arguments means serve, which is the default. Anything unrecognised prints
// the usage text and returns 2.
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

// runServe runs the HTTP service until the process is signalled, then shuts both
// listeners down within cfg.ShutdownTimeout.
//
// Startup is fail-fast for everything this process can check locally and patient about
// everything it cannot. Postgres and the blob root are checked before the readiness
// flag is set, so a misconfigured container dies immediately with a specific message
// rather than serving errors. PHP Admin Auth is the exception, exactly as it is for
// Config: an unavailable JWKS is not a reason to refuse to start, because the endpoint
// may not be deployed yet — Patch starts, rejects every restricted-channel token until
// keys arrive, and reports itself unready in the meantime. Manifests are the other
// exception: a Config database with no channel heads yet, or a transient read failure,
// must not crash-loop Patch. The initial LoadAll is attempted with a timeout, its
// failure is logged, and /readyz stays 503 until a later retry succeeds (PAT-B4).
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
	log.Info("patch starting",
		slog.String("version", version),
		slog.String("listen_addr", cfg.ListenAddr),
		slog.String("metrics_addr", cfg.MetricsAddr),
		slog.String("internal_addr", cfg.InternalAddr),
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

	// Patch never writes to the volume, and the image mounts it read-only. The check
	// here is only that it exists and is a directory: a missing volume would make
	// every blob request a 404, which is worth failing at start-up over.
	blobs, err := blob.NewRoot(cfg.BlobRoot)
	if err != nil {
		log.Error("blob root unusable at startup", slog.Any("error", err))
		return 1
	}

	// Start is deliberately before the verifier gates readiness: it returns
	// immediately and fetches in the background, so the boot timeout above only ever
	// covers Postgres. The verifier guards no route yet; PAT-B6 wires it to the dev
	// and staging manifest routes.
	verifier := auth.NewVerifier(cfg.StaffJWKSURL, cfg.StaffIssuer, cfg.StaffAudience, cfg.JWKSRefresh, cfg.JWTClockSkew)
	verifier.Start(ctx)

	cancelBoot()

	// The D4 key the internal listener accepts (CF-3). A path that is set but unusable
	// is a start-up error; no path at all leaves every internal route at 401, so a host
	// that has not generated patch_session.key yet still serves players.
	var serviceKeys *servicekey.Keys
	if cfg.SessionKeyPath != "" {
		serviceKeys, err = servicekey.LoadKeys(map[servicekey.Role]string{
			servicekey.RoleSession: cfg.SessionKeyPath,
		})
		if err != nil {
			log.Error("service keys unusable at startup", slog.Any("error", err))
			return 1
		}
	} else {
		log.Warn("PATCH_SESSION_KEY_PATH is not set; the internal server manifest routes refuse every caller")
	}

	holder := &manifest.Holder{}
	loader := &manifest.Loader{DB: db.Pool, Log: log, Holder: holder}
	watcher := &manifest.Watcher{
		Loader:       loader,
		Pool:         db.Pool,
		Log:          log,
		PollInterval: cfg.PollInterval,
	}

	srv := server.New(cfg, server.Deps{
		Ready: readyCheck(db, blobs, verifier),
		Manifests: func() error {
			return errors.Join(holder.Ready(), watcher.Ready())
		},
		ManifestHolder: holder,
		ServiceKeys:    serviceKeys,
		Verifier:       verifier,
		BlobRoot:       blobs,
		Version:        version,
		Logger:         log,
	})
	loader.OnPublish = func(e manifest.Entry) {
		srv.SetCurrentRelease(e.Channel, e.ReleaseID)
	}
	watcher.OnReload = srv.RecordManifestReload
	srv.SetReady(true)

	// A first load is attempted here, but a failure is not fatal: Config may not have
	// published anything yet, and PAT-B4's poll retries until it does.
	loadCtx, cancelLoad := context.WithTimeout(ctx, manifestLoadTimeout)
	if err := loader.LoadAll(loadCtx); err != nil {
		log.Error("initial manifest load failed; /readyz stays 503 until a retry succeeds",
			slog.Any("error", err))
	}
	cancelLoad()

	// The watcher keeps manifests fresh from here on: NOTIFY gives sub-second
	// propagation, the poll is the safety net for a missed notification, and a
	// reconnect reloads everything. It runs on the main context so a SIGTERM stops it.
	go watcher.Run(ctx)

	if err := srv.Run(ctx); err != nil {
		log.Error("listener stopped with an error", slog.Any("error", err))
		return 1
	}
	log.Info("shutdown complete")
	return 0
}

// readyCheck composes the three start-up dependencies /readyz must be honest about:
// Postgres reachable, the blob root still a directory, and at least one staff key
// fetched. The manifest check is supplied separately as holder.Ready, because it reads
// in-memory state rather than probing an external service.
//
// All three are cached or cheap by construction — the database probe is rate-limited
// inside store.DB, the blob check is a single stat, and the key check reads an
// in-memory map — because a load balancer may call this every second. The three are
// joined rather than short-circuited so the response names every reason at once; an
// operator fixing one problem should not have to discover the next by waiting for
// another poll.
func readyCheck(db *store.DB, blobs *blob.Root, verifier *auth.Verifier) func(context.Context) error {
	return func(ctx context.Context) error {
		var errs []error
		if err := db.Ready(ctx); err != nil {
			errs = append(errs, err)
		}
		if err := blobs.Ready(); err != nil {
			errs = append(errs, err)
		}
		// Not ready until PHP Admin Auth's JWKS has been fetched once. Until then this
		// service cannot verify a single restricted-channel token, so reporting ready
		// would let a load balancer send it traffic it can only answer with 401s.
		if !verifier.Ready(ctx) {
			errs = append(errs, fmt.Errorf("staff jwks not fetched yet from %s", verifier.URL()))
		}
		return errors.Join(errs...)
	}
}

// parse parses the serve flags. It returns -1 when the caller should carry on, or an
// exit code to return immediately: 0 for an explicit -h/-help, 2 for a malformed flag
// or a stray non-flag argument.
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
