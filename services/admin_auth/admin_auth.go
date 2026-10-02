// Command admin_auth is the Otomo staff-identity service: it owns staff accounts, the
// Ed25519 staff signing keys, access-token issuance, and the JWKS that Gateway and
// every other service verify staff tokens against (issuer
// https://admin-auth.otomo.internal, audience otomo:staff).
//
// One binary, four commands. serve runs the HTTP service, migrate applies the
// embedded goose migrations, genkey generates a signing key and registers it, and
// bootstrap-root creates or rotates the single break-glass root account.
// Migrations, key generation and bootstrap are deliberately separate one-shot steps
// rather than startup work: a service that generated its key at startup would
// invalidate every outstanding token on every restart.
package main

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/otomo-live/otomo/services/admin_auth/internal/bootstrap"
	"github.com/otomo-live/otomo/services/admin_auth/internal/config"
	"github.com/otomo-live/otomo/services/admin_auth/internal/mfa"
	"github.com/otomo-live/otomo/services/admin_auth/internal/server"
	"github.com/otomo-live/otomo/services/admin_auth/internal/store"
	"github.com/otomo-live/otomo/services/admin_auth/internal/token"
	"github.com/otomo-live/otomo/services/admin_auth/migrations"
)

// version is the build identifier reported in the startup log line and as
// admin_auth_build_info on /metrics. The Docker build stamps it with
// -X main.version=${GIT_SHA}; a plain `go build` leaves the "dev" default.
var version = "dev"

const (
	dbPingTimeout = 5 * time.Second

	usage = `usage: admin_auth [command]

commands:
  serve           run the HTTP service (default)
  migrate         apply the embedded database migrations
  genkey          generate an Ed25519 signing key and register it in the database
  bootstrap-root  create or rotate the single break-glass root account

flags for genkey:
  -kid  signing key ID (default: admin-auth-<current UTC date>)
  -out  path to write the PKCS#8 PEM private key (default: $ADMIN_AUTH_SIGNING_KEY_PATH)
  -totp path to write a new 32-byte base64 TOTP key instead, and exit

flags for bootstrap-root:
  -email   root account email (default: root@otomo.internal)
  -name    root account display name (default: Root)
  -rotate  rotate the existing root password instead of leaving it alone
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
	case "bootstrap-root":
		return runBootstrapRoot(args[1:])
	default:
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
}

// runServe runs the HTTP service until the process is signalled, then shuts both
// listeners down within cfg.ShutdownTimeout.
//
// Startup is fail-fast and ordered. Everything the service needs to serve is checked
// before the readiness flag is set — Postgres must answer, the key file must parse,
// and the file's public key must have an active signing_key row — so a misconfigured
// container fails loudly at startup instead of serving 500s or, worse, tokens signed
// by a key no verifier can find.
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
	log.Info("admin_auth starting",
		slog.String("version", version),
		slog.String("listen_addr", cfg.ListenAddr),
		slog.String("metrics_addr", cfg.MetricsAddr),
		slog.String("issuer", cfg.Issuer),
		slog.String("audience", cfg.Audience),
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

	priv, err := token.Load(cfg.SigningKeyPath)
	if err != nil {
		log.Error("cannot load signing key", slog.Any("error", err))
		return 1
	}
	pub := token.Public(priv)

	totpKey, err := mfa.LoadKey(cfg.TOTPKeyPath)
	if err != nil {
		log.Error("cannot load TOTP key",
			slog.String("path", cfg.TOTPKeyPath),
			slog.String("variable", "ADMIN_AUTH_TOTP_KEY_PATH"),
			slog.Any("error", err),
		)
		return 1
	}

	kid, err := db.FindActiveByPublicKey(bootCtx, pub)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Error("signing key at ADMIN_AUTH_SIGNING_KEY_PATH has no active row in signing_key; run `admin_auth genkey`",
				slog.String("path", cfg.SigningKeyPath))
		} else {
			log.Error("cannot look up the signing key", slog.Any("error", err))
		}
		return 1
	}

	active, err := db.ListActiveSigningKeys(bootCtx)
	if err != nil {
		log.Error("cannot list active signing keys", slog.Any("error", err))
		return 1
	}
	raw, err := token.BuildJWKS(publicKeys(active))
	if err != nil {
		log.Error("cannot build the JWKS document", slog.Any("error", err))
		return 1
	}

	jwks := &token.JWKSCache{}
	jwks.Swap(raw)
	cancelBoot()

	signer := &token.Signer{
		Kid:        kid,
		PrivateKey: priv,
		Issuer:     cfg.Issuer,
		Audience:   cfg.Audience,
		TTL:        cfg.AccessTokenTTL,
	}

	// /me verifies access tokens in-process against the same active key set the JWKS
	// publishes, so a token this service signed is one this service can check.
	verifier := token.NewVerifier(cfg.Issuer, cfg.Audience, publicKeys(active))

	srv := server.New(cfg, server.Deps{
		JWKS:     jwks,
		Signer:   signer,
		Verifier: verifier,
		Store:    db,
		Ready:    db.Ready,
		MFAKey:   totpKey,
		// The real signing-key check: the JWKS cache is only ever populated
		// with at least one key, because a startup without one exits above.
		// Keeping the check on the ready path means a future cache bug cannot
		// leave the service reporting ready with nothing to verify against.
		Keys: func() error {
			if len(active) == 0 {
				return errors.New("no signing keys loaded")
			}
			return nil
		},
		Version:      version,
		JWKSKeyCount: len(active),
		Logger:       log,
	})
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
// It opens its own database/sql handle rather than the pgx pool, because goose wants
// a *sql.DB and this runs as a one-shot command, never alongside serve. Applying the
// same migrations twice is a no-op: goose tracks applied versions in its own table.
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

// runGenkey generates an Ed25519 key, writes it as a PKCS#8 PEM file with 0600
// permissions, inserts the matching signing_key row, and prints the kid and path.
//
// The private key is written before the row is inserted and removed again if that
// insert fails, so a failed run leaves no orphan key on disk and can be retried with
// the same -kid. A file that already exists at -out is never overwritten.
func runGenkey(args []string) int {
	flags := flag.NewFlagSet("genkey", flag.ContinueOnError)
	kid := flags.String("kid", "admin-auth-"+time.Now().UTC().Format("2006-01-02"), "signing key ID")
	out := flags.String("out", "", "path to write the PKCS#8 PEM private key")
	totpOut := flags.String("totp", "", "path to write a new 32-byte base64 TOTP key instead of a signing key")
	if code := parse(flags, args); code >= 0 {
		return code
	}

	// Generating the symmetric TOTP key is a purely local operation: the key is
	// consumed by `serve`, not registered anywhere, so this branch runs before any
	// database or signing-key configuration is loaded.
	if *totpOut != "" {
		return runGenkeyTOTP(*totpOut)
	}

	cfg, err := config.Load(config.ModeGenkey)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	log := newLogger(cfg.LogLevel)

	path := *out
	if path == "" {
		path = cfg.SigningKeyPath
	}
	if path == "" {
		fmt.Fprintln(os.Stderr, "genkey: -out is required when ADMIN_AUTH_SIGNING_KEY_PATH is unset")
		return 2
	}
	if *kid == "" {
		fmt.Fprintln(os.Stderr, "genkey: -kid must not be empty")
		return 2
	}
	if _, err := os.Stat(path); err == nil {
		fmt.Fprintf(os.Stderr, "genkey: %s already exists; refusing to overwrite it\n", path)
		return 1
	} else if !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "genkey: cannot stat %s: %v\n", path, err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), dbPingTimeout)
	defer cancel()

	db, err := store.NewPool(ctx, cfg)
	if err != nil {
		log.Error("postgres unusable", slog.Any("error", err))
		return 1
	}
	defer db.Close()

	pub, priv, err := token.Generate()
	if err != nil {
		log.Error("cannot generate a key", slog.Any("error", err))
		return 1
	}
	if err := token.Write(path, priv); err != nil {
		log.Error("cannot write the key file", slog.Any("error", err))
		return 1
	}
	if err := db.InsertSigningKey(ctx, *kid, pub); err != nil {
		// Leave no private key on disk that no `signing_key` row refers to:
		// the operator must be able to rerun with the same -kid.
		if rmErr := os.Remove(path); rmErr != nil {
			log.Error("cannot remove the key file after a failed insert",
				slog.String("path", path), slog.Any("error", rmErr))
		}
		log.Error("cannot register the signing key; the key file was removed so this can be retried",
			slog.String("kid", *kid), slog.Any("error", err))
		return 1
	}

	fmt.Printf("kid:  %s\npath: %s\n", *kid, path)
	return 0
}

// runGenkeyTOTP writes a new 32-byte TOTP key as a base64 line with mode 0600. It
// refuses to overwrite an existing file, which is what makes a rerun safe: an operator
// who points at the wrong path loses no key. It needs no database because the key is
// only ever read by this service at serve time.
func runGenkeyTOTP(path string) int {
	key, err := mfa.GenerateKey()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := mfa.WriteKey(path, key); err != nil {
		if errors.Is(err, mfa.ErrKeyExists) {
			fmt.Fprintf(os.Stderr, "genkey: %s already exists; refusing to overwrite it\n", path)
			return 1
		}
		fmt.Fprintf(os.Stderr, "genkey: %v\n", err)
		return 1
	}
	fmt.Printf("totp key: %s\n", path)
	return 0
}

// runBootstrapRoot creates the single break-glass root account, or rotates its
// password with -rotate. The password comes from ADMIN_AUTH_ROOT_PASSWORD_FILE; it is
// hashed before it reaches the database and is never printed.
//
// It needs only ADMIN_AUTH_DATABASE_URL, like migrate: the root account exists before
// any signing key is registered, so requiring the serve key would make the very first
// bootstrap impossible.
func runBootstrapRoot(args []string) int {
	flags := flag.NewFlagSet("bootstrap-root", flag.ContinueOnError)
	email := flags.String("email", bootstrap.DefaultEmail, "email for the root account")
	name := flags.String("name", bootstrap.DefaultName, "display name for the root account")
	rotate := flags.Bool("rotate", false, "rotate the existing root password")
	if code := parse(flags, args); code >= 0 {
		return code
	}

	cfg, err := config.Load(config.ModeBootstrapRoot)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), dbPingTimeout)
	defer cancel()

	db, err := store.NewPool(ctx, cfg)
	if err != nil {
		newLogger(cfg.LogLevel).Error("postgres unusable", slog.Any("error", err))
		return 1
	}
	defer db.Close()

	msg, err := bootstrap.Run(ctx, db, bootstrap.Options{
		Email:        *email,
		Name:         *name,
		Rotate:       *rotate,
		PasswordFile: cfg.RootPasswordFile,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(msg)
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

// newLogger returns the process-wide JSON logger: one JSON object per line on
// stdout at the configured level. Logs go to stdout rather than stderr so the
// container log stream is a single ordered sequence.
func newLogger(level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}

// publicKeys converts stored signing_key rows into the JWKS builder's input type.
func publicKeys(keys []store.SigningKey) []token.PublicKey {
	out := make([]token.PublicKey, 0, len(keys))
	for _, k := range keys {
		out = append(out, token.PublicKey{Kid: k.Kid, Key: ed25519.PublicKey(k.PublicKey)})
	}
	return out
}
