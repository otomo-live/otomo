// Package config reads and validates the Patch service's configuration from the
// environment.
//
// Load runs once at start-up and reports every problem at once rather than stopping at
// the first, so a single start-up tells an operator everything wrong with a
// container's environment. Durations are Go duration strings ("30s", "15m") and the
// log level is a slog name (debug, info, warn, error).
package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully resolved configuration. Every field is filled in by Load, from
// its environment variable or from that variable's default; no field is left for a
// caller to supply.
//
// There is deliberately no field for the staff signing key. Patch verifies the staff
// tokens that guard dev and staging manifests against Admin Auth's JWKS and never
// mints one, so the private key exists only inside PHP Admin Auth.
type Config struct {
	ListenAddr  string // PATCH_LISTEN_ADDR — the public listener
	MetricsAddr string // PATCH_METRICS_ADDR — health, metrics and pprof
	DatabaseURL string // PATCH_DATABASE_URL — the read-only patch_ro role
	DBMaxConns  int32  // PATCH_DB_MAX_CONNS
	BlobRoot    string // PATCH_BLOB_ROOT — Config's blob volume, mounted read-only

	// InternalAddr is the listener for internal callers only (CF-3): the server
	// manifest and its blobs, behind a D4 service key. No gateway routes to it.
	InternalAddr string // PATCH_INTERNAL_ADDR
	// SessionKeyPath is the file holding the key Session presents (patch_session.key).
	// Empty means no caller is accepted: every internal route answers 401.
	SessionKeyPath string // PATCH_SESSION_KEY_PATH

	// The staff identity domain, from 06-auth-identity-contract.md §3 and §5. Both
	// values are checked against the token's own claims, never inferred from which
	// JWKS answered. They are only needed once PAT-B6 restricts dev and staging.
	StaffJWKSURL  string // PATCH_STAFF_JWKS_URL — PHP Admin Auth's JWKS
	StaffIssuer   string // PATCH_STAFF_ISSUER
	StaffAudience string // PATCH_STAFF_AUDIENCE

	JWKSRefresh     time.Duration // PATCH_STAFF_JWKS_REFRESH
	JWTClockSkew    time.Duration // PATCH_JWT_CLOCK_SKEW
	PollInterval    time.Duration // PATCH_POLL_INTERVAL — used by PAT-B4's fallback poll
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
	LogLevel        slog.Level
}

// Load builds the Config, reading each variable through the helpers below.
//
// A malformed or non-positive value is not fatal on its own: its default is used and
// the problem recorded, so the returned error names every variable that needs fixing
// at once instead of one per restart. Missing required variables are collected the
// same way.
func Load() (Config, error) {
	var problems []string

	cfg := Config{
		ListenAddr:      stringVar("PATCH_LISTEN_ADDR", ":8080"),
		MetricsAddr:     stringVar("PATCH_METRICS_ADDR", ":9090"),
		DatabaseURL:     stringVar("PATCH_DATABASE_URL", ""),
		BlobRoot:        stringVar("PATCH_BLOB_ROOT", ""),
		InternalAddr:    stringVar("PATCH_INTERNAL_ADDR", ":8081"),
		SessionKeyPath:  stringVar("PATCH_SESSION_KEY_PATH", ""),
		StaffJWKSURL:    stringVar("PATCH_STAFF_JWKS_URL", ""),
		StaffIssuer:     stringVar("PATCH_STAFF_ISSUER", "https://admin-auth.otomo.internal"),
		StaffAudience:   stringVar("PATCH_STAFF_AUDIENCE", "otomo:staff"),
		DBMaxConns:      intVar("PATCH_DB_MAX_CONNS", 4, &problems),
		JWKSRefresh:     durVar("PATCH_STAFF_JWKS_REFRESH", 30*time.Second, &problems),
		JWTClockSkew:    skewVar("PATCH_JWT_CLOCK_SKEW", 30*time.Second, &problems),
		PollInterval:    durVar("PATCH_POLL_INTERVAL", 60*time.Second, &problems),
		ReadTimeout:     durVar("PATCH_READ_TIMEOUT", 10*time.Second, &problems),
		WriteTimeout:    durVar("PATCH_WRITE_TIMEOUT", 30*time.Second, &problems),
		IdleTimeout:     durVar("PATCH_IDLE_TIMEOUT", 120*time.Second, &problems),
		ShutdownTimeout: durVar("PATCH_SHUTDOWN_TIMEOUT", 15*time.Second, &problems),
		LogLevel:        levelVar("PATCH_LOG_LEVEL", slog.LevelInfo, &problems),
	}

	// Patch owns no schema and runs no migrate command, so every requirement is a
	// serve requirement.
	if cfg.DatabaseURL == "" {
		problems = append(problems, "PATCH_DATABASE_URL is required")
	}
	if cfg.BlobRoot == "" {
		problems = append(problems, "PATCH_BLOB_ROOT is required")
	}
	if cfg.StaffJWKSURL == "" {
		problems = append(problems, "PATCH_STAFF_JWKS_URL is required")
	} else if u, err := url.Parse(cfg.StaffJWKSURL); err != nil || u.Scheme == "" || u.Host == "" {
		// A JWKS URL with a typo in it is indistinguishable from PHP Admin Auth being
		// down once the process is running. Checking the shape here turns that into a
		// start-up error.
		problems = append(problems, fmt.Sprintf("PATCH_STAFF_JWKS_URL %q is not an absolute URL", cfg.StaffJWKSURL))
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

// durVar parses key as a positive Go duration ("30s", "15m"), recording a problem
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

// skewVar is durVar for PATCH_JWT_CLOCK_SKEW, with the one difference that zero is
// accepted. It has to be: a deployment whose containers share a clock source has no
// use for leeway, and refusing 0 would force it to state a small nonzero value that
// means something different from what it intends. Timeouts keep the positive-only
// rule, because there 0 means "no timeout" rather than "no leeway".
func skewVar(key string, def time.Duration, problems *[]string) time.Duration {
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
