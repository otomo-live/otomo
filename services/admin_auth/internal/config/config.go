// Package config reads and validates the service's configuration from the
// environment.
//
// Load runs once per command and reports every problem at once rather than stopping
// at the first, so a single start-up tells an operator everything wrong with a
// container's environment. Durations are Go duration strings ("15m", "720h") and the
// log level is a slog name (debug, info, warn, error).
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/otomo-live/otomo/services/admin_auth/internal/mfa"
)

// Mode names the command a configuration is being loaded for. It is kept even though
// serve and migrate currently require the same variables, so a future command that
// needs a different subset has an obvious place to say so.
type Mode string

const (
	ModeServe         Mode = "serve"          // long-running HTTP service
	ModeMigrate       Mode = "migrate"        // one-shot schema migration
	ModeGenkey        Mode = "genkey"         // one-shot signing-key generation
	ModeBootstrapRoot Mode = "bootstrap-root" // one-shot root-account bootstrap
)

// maxRefreshTokenTTL caps the refresh-cookie lifetime at 30 days. A refresh token
// that outlives a month is a credential an attacker can keep using long after the
// staff member has forgotten the device they logged in on.
const maxRefreshTokenTTL = 720 * time.Hour

// maxRefreshReuseGrace caps the window in which presenting an already-rotated
// refresh token is treated as a benign multi-tab race rather than a theft. A minute
// is already generous for two tabs whose access tokens expired together; five is the
// hard ceiling so a misconfiguration cannot turn reuse detection off for long.
const maxRefreshReuseGrace = 5 * time.Minute

// minInviteTTL and maxInviteTTL bound the invite and password-reset link lifetime.
// A link shorter than an hour is unlikely to reach anyone; one longer than a week is
// an unused credential sitting in a mailbox for too long.
const (
	minInviteTTL = time.Hour
	maxInviteTTL = 168 * time.Hour
)

// Config is the fully resolved configuration for one command. Every field is filled
// in by Load, from its environment variable or from that variable's default; no field
// is left for a caller to supply.
type Config struct {
	ListenAddr        string // ADMIN_AUTH_LISTEN_ADDR — the public listener
	MetricsAddr       string // ADMIN_AUTH_METRICS_ADDR — health, metrics and pprof
	DatabaseURL       string // ADMIN_AUTH_DATABASE_URL
	DBMaxConns        int32  // ADMIN_AUTH_DB_MAX_CONNS
	SigningKeyPath    string // ADMIN_AUTH_SIGNING_KEY_PATH — a PKCS#8 PEM private key
	TOTPKeyPath       string // ADMIN_AUTH_TOTP_KEY_PATH — a base64 32-byte AES key
	RootPasswordFile  string // ADMIN_AUTH_ROOT_PASSWORD_FILE — the break-glass root password
	Issuer            string // ADMIN_AUTH_ISSUER — the iss claim on every staff token
	Audience          string // ADMIN_AUTH_AUDIENCE — the aud claim on every staff token
	AccessTokenTTL    time.Duration
	RefreshTokenTTL   time.Duration // ADMIN_AUTH_REFRESH_TOKEN_TTL — refresh cookie lifetime
	RefreshReuseGrace time.Duration // ADMIN_AUTH_REFRESH_REUSE_GRACE — benign reuse window
	InviteTTL         time.Duration // ADMIN_AUTH_INVITE_TTL — invite/reset link lifetime
	PublicURL         string        // ADMIN_AUTH_PUBLIC_URL — base for onboarding links
	LoginMaxFailures  int           // ADMIN_AUTH_LOGIN_MAX_FAILURES — failures before a lockout
	LoginLockout      time.Duration // ADMIN_AUTH_LOGIN_LOCKOUT — how long a lockout lasts
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
	LogLevel          slog.Level
}

// Load builds the Config for mode, reading each variable through the helpers below.
//
// A malformed or non-positive value is not fatal on its own: its default is used and
// the problem recorded, so the returned error names every variable that needs fixing
// at once instead of one per restart. Missing required variables are collected the
// same way.
func Load(mode Mode) (Config, error) {
	var problems []string

	cfg := Config{
		ListenAddr:        stringVar("ADMIN_AUTH_LISTEN_ADDR", ":8080"),
		MetricsAddr:       stringVar("ADMIN_AUTH_METRICS_ADDR", ":9090"),
		DatabaseURL:       stringVar("ADMIN_AUTH_DATABASE_URL", ""),
		SigningKeyPath:    stringVar("ADMIN_AUTH_SIGNING_KEY_PATH", ""),
		TOTPKeyPath:       stringVar("ADMIN_AUTH_TOTP_KEY_PATH", ""),
		RootPasswordFile:  stringVar("ADMIN_AUTH_ROOT_PASSWORD_FILE", "/run/secrets/admin_root_password"),
		Issuer:            stringVar("ADMIN_AUTH_ISSUER", "https://admin-auth.otomo.internal"),
		Audience:          stringVar("ADMIN_AUTH_AUDIENCE", "otomo:staff"),
		DBMaxConns:        intVar("ADMIN_AUTH_DB_MAX_CONNS", 8, &problems),
		AccessTokenTTL:    durVar("ADMIN_AUTH_ACCESS_TOKEN_TTL", 15*time.Minute, &problems),
		RefreshTokenTTL:   durVar("ADMIN_AUTH_REFRESH_TOKEN_TTL", 168*time.Hour, &problems),
		RefreshReuseGrace: graceVar("ADMIN_AUTH_REFRESH_REUSE_GRACE", 30*time.Second, maxRefreshReuseGrace, &problems),
		InviteTTL:         durVar("ADMIN_AUTH_INVITE_TTL", 72*time.Hour, &problems),
		PublicURL:         stringVar("ADMIN_AUTH_PUBLIC_URL", "http://localhost:8090"),
		LoginMaxFailures:  int(intVar("ADMIN_AUTH_LOGIN_MAX_FAILURES", 5, &problems)),
		LoginLockout:      durVar("ADMIN_AUTH_LOGIN_LOCKOUT", 15*time.Minute, &problems),
		ReadTimeout:       durVar("ADMIN_AUTH_READ_TIMEOUT", 10*time.Second, &problems),
		WriteTimeout:      durVar("ADMIN_AUTH_WRITE_TIMEOUT", 30*time.Second, &problems),
		IdleTimeout:       durVar("ADMIN_AUTH_IDLE_TIMEOUT", 120*time.Second, &problems),
		ShutdownTimeout:   durVar("ADMIN_AUTH_SHUTDOWN_TIMEOUT", 15*time.Second, &problems),
		LogLevel:          levelVar("ADMIN_AUTH_LOG_LEVEL", slog.LevelInfo, &problems),
	}

	if cfg.DatabaseURL == "" {
		problems = append(problems, "ADMIN_AUTH_DATABASE_URL is required")
	}
	if cfg.SigningKeyPath == "" && mode == ModeServe {
		problems = append(problems, "ADMIN_AUTH_SIGNING_KEY_PATH is required")
	}
	if mode == ModeServe {
		if cfg.TOTPKeyPath == "" {
			problems = append(problems, "ADMIN_AUTH_TOTP_KEY_PATH is required")
		} else if _, err := mfa.LoadKey(cfg.TOTPKeyPath); err != nil {
			// The key is validated here, not only in runServe, so a bad file is
			// reported with every other configuration problem in one boot message
			// and names the variable rather than the path alone.
			problems = append(problems, fmt.Sprintf("ADMIN_AUTH_TOTP_KEY_PATH %s is not a valid TOTP key: %v", cfg.TOTPKeyPath, err))
		}
	}
	if cfg.AccessTokenTTL > time.Hour {
		problems = append(problems, fmt.Sprintf("ADMIN_AUTH_ACCESS_TOKEN_TTL %s must be at most 1h", cfg.AccessTokenTTL))
	}
	if cfg.RefreshTokenTTL > maxRefreshTokenTTL {
		problems = append(problems, fmt.Sprintf("ADMIN_AUTH_REFRESH_TOKEN_TTL %s must be at most %s", cfg.RefreshTokenTTL, maxRefreshTokenTTL))
	}
	if cfg.InviteTTL < minInviteTTL || cfg.InviteTTL > maxInviteTTL {
		problems = append(problems, fmt.Sprintf("ADMIN_AUTH_INVITE_TTL %s must be between %s and %s", cfg.InviteTTL, minInviteTTL, maxInviteTTL))
	}

	if len(problems) > 0 {
		return Config{}, fmt.Errorf("invalid configuration: %s", strings.Join(problems, "; "))
	}
	return cfg, nil
}

// stringVar returns the value of key, or def when it is unset or empty. Treating an
// empty value as unset means an environment variable set to "" cannot silently blank
// out a default.
func stringVar(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// intVar parses key as a positive int32, recording a problem and returning def when
// it is not. Zero and negative values are rejected: neither makes sense for a count
// of connections.
func intVar(key string, def int32, problems *[]string) int32 {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || n <= 0 {
		*problems = append(*problems, fmt.Sprintf("%s %q is not a positive integer", key, raw))
		return def
	}
	return int32(n)
}

// durVar parses key as a positive Go duration ("15m", "720h"), recording a problem
// and returning def when it is not. A zero or negative timeout is rejected rather
// than passed to net/http, where it would mean "no timeout" rather than "instant".
func durVar(key string, def time.Duration, problems *[]string) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		*problems = append(*problems, fmt.Sprintf("%s %q is not a duration", key, raw))
		return def
	}
	if d <= 0 {
		*problems = append(*problems, fmt.Sprintf("%s %q must be positive", key, raw))
		return def
	}
	return d
}

// graceVar parses key as a non-negative Go duration no greater than max. Zero is a
// valid value — it disables the benign-reuse window — so this cannot use durVar,
// which rejects every non-positive duration. A value over the cap is refused rather
// than clamped: silently shortening an operator's grace period would change auth
// behaviour without saying so.
func graceVar(key string, def, max time.Duration, problems *[]string) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		*problems = append(*problems, fmt.Sprintf("%s %q is not a duration", key, raw))
		return def
	}
	if d < 0 {
		*problems = append(*problems, fmt.Sprintf("%s %q must not be negative", key, raw))
		return def
	}
	if d > max {
		*problems = append(*problems, fmt.Sprintf("%s %q must be at most %s", key, raw, max))
		return def
	}
	return d
}

// levelVar parses key as a slog level name, recording a problem and returning def
// when it is not. Go's own UnmarshalText defines the accepted spellings, so this
// accepts exactly what slog accepts and nothing extra.
func levelVar(key string, def slog.Level, problems *[]string) slog.Level {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(raw)); err != nil {
		*problems = append(*problems, fmt.Sprintf("%s %q is not a log level (debug, info, warn, error)", key, raw))
		return def
	}
	return lvl
}
