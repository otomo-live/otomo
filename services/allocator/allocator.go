// Command allocator is the Otomo game-server allocation service.
//
// The Allocator is internal-only: it has no gateway route and no public API. It owns
// the game-server pool and the allocations made from it, and it mints the short-lived
// join tickets a player presents to a game server. Session asks it for a server on
// behalf of a party, and every game server registers and heartbeats with it. The design
// is in design/14-launch-handoff.md.
//
// One binary, three commands. serve runs the HTTP service, migrate applies the
// embedded goose migrations, and genkey writes a fresh join-ticket signing key.
// Deps.Routes is the seam the registry and allocation tickets fill.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/otomo-live/otomo/services/allocator/internal/api"
	"github.com/otomo-live/otomo/services/allocator/internal/callback"
	"github.com/otomo-live/otomo/services/allocator/internal/config"
	"github.com/otomo-live/otomo/services/allocator/internal/metrics"
	"github.com/otomo-live/otomo/services/allocator/internal/pool"
	"github.com/otomo-live/otomo/services/allocator/internal/server"
	"github.com/otomo-live/otomo/services/allocator/internal/servicekey"
	"github.com/otomo-live/otomo/services/allocator/internal/store"
	"github.com/otomo-live/otomo/services/allocator/internal/token"
	"github.com/otomo-live/otomo/services/allocator/migrations"
)

// version is the build identifier reported in the startup log line and as
// allocator_build_info on /metrics. The Docker build stamps it with
// -X main.version=${GIT_SHA}; a plain `go build` leaves the "dev" default.
var version = "dev"

const (
	dbPingTimeout = 5 * time.Second

	usage = `usage: allocator [command]

commands:
  serve    run the HTTP service (default)
  migrate  apply the embedded database migrations
  genkey   generate the join-ticket Ed25519 signing key

flags for genkey:
  -out  path to write the PKCS#8 PEM private key (default: $ALLOCATOR_SIGNING_KEY_PATH,
        else /run/secrets/allocator/signing_key.pem)
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
	case "genkey":
		return runGenkey(args[1:])
	default:
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
}

// runServe runs the HTTP service until the process is signalled, then shuts both
// listeners down within cfg.ShutdownTimeout.
//
// Startup is fail-fast and ordered. Everything the service needs to serve is checked
// before the readiness flag is set — Postgres must answer, every service key must load,
// and the join-ticket signing key must parse — so a misconfigured container fails
// loudly at startup instead of serving 500s. The public half of the signing key is
// published through Deps.Routes; the allocation routes mint tickets with it.
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
	log.Info("allocator starting",
		slog.String("version", version),
		slog.String("listen_addr", cfg.ListenAddr),
		slog.String("metrics_addr", cfg.MetricsAddr),
		slog.String("public_addr", net.JoinHostPort(cfg.PublicAddress, strconv.Itoa(cfg.PublicPort))),
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

	// Session's key authorises allocation requests, the game-server key authorises
	// registration and heartbeats, and the proxy's key authorises the pool read the
	// Gameplay Proxy uses to resolve a ticket's srv claim. Loading all three here turns
	// a missing or malformed file into a start-up failure rather than a 401 on the
	// first real request; the error names the file, so an operator knows which secret
	// to regenerate.
	keys, err := servicekey.LoadKeys(map[servicekey.Role]string{
		servicekey.RoleSession:    cfg.SessionKeyPath,
		servicekey.RoleGameServer: cfg.GameServerKeyPath,
		servicekey.RoleProxy:      cfg.ProxyKeyPath,
	})
	if err != nil {
		log.Error("cannot load service keys; run deploy/scripts/generate-secrets.sh to create them",
			slog.Any("error", err))
		return 1
	}

	// The callback key is the one secret the Allocator spends rather than accepts: it
	// is presented to Session when an allocation ends. Loading it here, with the same
	// validation as the keys it accepts, turns a missing or malformed file into a
	// start-up failure instead of a callback that would fail every retry.
	callbackKey, err := servicekey.ReadKeyFile(cfg.CallbackKeyPath)
	if err != nil {
		log.Error("cannot load the Session callback key; run deploy/scripts/generate-secrets.sh to create it",
			slog.Any("error", err))
		return 1
	}

	// The signing key mints the join tickets. A missing or malformed file fails
	// start-up here rather than on the first allocation, and the error names the path.
	priv, err := token.Load(cfg.SigningKeyPath)
	if err != nil {
		log.Error("cannot load the join-ticket signing key; generate one with `allocator genkey`",
			slog.Any("error", err))
		return 1
	}
	issuer := token.NewIssuer(priv, cfg.Issuer, cfg.Audience, cfg.TicketTTL, nil)
	log.Info("join-ticket signing key loaded", slog.String("signing_kid", issuer.Kid()))

	cancelBoot()

	// The registry. The reaper marks servers dead after
	// HeartbeatTimeout without a heartbeat and ends their allocations; its SQL
	// locks the rows it changes, so a second Allocator instance running the same
	// loop is harmless rather than a double reap.
	registry := pool.New(db.Pool, pool.Timings{
		HeartbeatTimeout: cfg.HeartbeatTimeout,
		ReservationTTL:   cfg.ReservationTTL,
	})

	// The domain instruments register on the registry the server owns and
	// serves on /metrics. The server has to exist first to hand out that registry, and
	// srv.Run is what invokes Routes, so the forward-declared m is assigned before any
	// handler or background loop can read it.
	var m *metrics.Metrics
	srv := server.New(cfg, server.Deps{
		Ready:   db.Ready,
		Version: version,
		Logger:  log,
		// The JWKS carries only the public half of the signing key and holds no
		// secret, and game servers and the Gameplay Proxy fetch it to verify
		// tickets, so this route is deliberately not behind the service-key auth.
		Routes: func(mux *http.ServeMux) {
			mux.Handle("GET /.well-known/jwks.json", token.Handler(token.JWKS(token.Public(priv))))
			api.RegisterServerRoutes(mux, keys, registry)
			// Session's routes. Clients are always sent to the Gameplay
			// Proxy's public address, never a game server's (decision D3).
			api.RegisterAllocationRoutes(mux, keys, registry, issuer, cfg.PublicAddress, cfg.PublicPort, m, log)
		},
	})
	m = metrics.New(srv.Registry(), db.Pool)

	// The Session callback drain shares the outbox with the reaper:
	// whichever path ends an allocation flags it callback_pending, and this loop is
	// what tells Session, retrying with a backoff and giving up to Session's repair
	// poll after five attempts. Both loops run on the same signal context, so they
	// stop together on shutdown, and both report their work through m.
	go registry.RunReaper(ctx, cfg.ReapInterval, cfg.HeartbeatTimeout, m, log)
	notifier := callback.New(db.Pool, cfg.SessionURL, callbackKey, &http.Client{Timeout: 10 * time.Second}, m, log)
	go notifier.Run(ctx, time.Second)

	srv.SetReady(true)

	if err := srv.Run(ctx); err != nil {
		log.Error("listener stopped with an error", slog.Any("error", err))
		return 1
	}
	log.Info("shutdown complete")
	return 0
}

// runMigrate applies the embedded goose migrations and returns.
//
// It opens its own database/sql handle rather than the pgx pool, because goose wants a
// *sql.DB and this runs as a one-shot command, never alongside serve. It loads only
// the database URL and log level: migrate must run before the key files are mounted
// and before ALLOCATOR_PUBLIC_ADDRESS is known, so it cannot go through config.Load.
// Applying the same migrations twice is a no-op: goose tracks applied versions in its
// own table.
func runMigrate(args []string) int {
	flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
	if code := parse(flags, args); code >= 0 {
		return code
	}

	cfg, err := config.LoadDatabase()
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

// runGenkey generates an Ed25519 key, writes it as a PKCS#8 PEM file with 0600
// permissions, and prints the path and the key's kid.
//
// It is a one-shot command with no database: the only input is the path to write to,
// from -out, then ALLOCATOR_SIGNING_KEY_PATH, then the default secrets mount. Like
// migrate it deliberately does not call config.Load, so it runs on a fresh host before
// the rest of the environment exists. A file that already exists at -out is never
// overwritten; rotate by generating a new file and restarting serve with the new path.
func runGenkey(args []string) int {
	defaultOut := os.Getenv("ALLOCATOR_SIGNING_KEY_PATH")
	if defaultOut == "" {
		defaultOut = "/run/secrets/allocator/signing_key.pem"
	}
	flags := flag.NewFlagSet("genkey", flag.ContinueOnError)
	out := flags.String("out", defaultOut, "path to write the PKCS#8 PEM private key")
	if code := parse(flags, args); code >= 0 {
		return code
	}
	log := newLogger(slog.LevelInfo)

	if _, err := os.Stat(*out); err == nil {
		fmt.Fprintf(os.Stderr, "genkey: %s already exists; refusing to overwrite it\n", *out)
		return 1
	} else if !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "genkey: cannot stat %s: %v\n", *out, err)
		return 1
	}

	pub, priv, err := token.Generate()
	if err != nil {
		log.Error("cannot generate a key", slog.Any("error", err))
		return 1
	}
	if err := token.Write(*out, priv); err != nil {
		log.Error("cannot write the key file", slog.Any("error", err))
		return 1
	}

	fmt.Printf("wrote %s (kid %s)\n", *out, token.Thumbprint(pub))
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
