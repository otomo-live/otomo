// Package config reads and validates the service's configuration from the
// environment.
//
// Load runs once per command and reports every problem at once rather than stopping
// at the first, so a single start-up tells an operator everything wrong with a
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

// Mode names the command a configuration is being loaded for. It matters because the
// commands need different things: only serve needs a blob volume to write into.
type Mode string

const (
	ModeServe   Mode = "serve"   // long-running HTTP service
	ModeMigrate Mode = "migrate" // one-shot schema migration
)

// Config is the fully resolved configuration for one command. Every field is filled
// in by Load, from its environment variable or from that variable's default; no field
// is left for a caller to supply.
//
// There is deliberately no field for the staff signing key. Config verifies tokens
// against a JWKS endpoint and never mints one, so the private key exists only inside
// PHP Admin Auth — the service cannot sign even if it is compromised.
type Config struct {
	ListenAddr  string // CONFIG_LISTEN_ADDR — the public listener
	MetricsAddr string // CONFIG_METRICS_ADDR — health, metrics and pprof
	DatabaseURL string // CONFIG_DATABASE_URL
	DBMaxConns  int32  // CONFIG_DB_MAX_CONNS
	BlobRoot    string // CONFIG_BLOB_ROOT — the volume shared with Patch's nginx

	// MaxPackBytes is the largest content pack body POST /packs will read.
	// CONFIG_MAX_PACK_BYTES; design/02-config.md §7 permits 512 MiB.
	MaxPackBytes int64

	// The staff identity domain, from 06-auth-identity-contract.md §3 and §5. Both
	// values are checked against the token's own claims, never inferred from which
	// JWKS answered.
	StaffJWKSURL  string // CONFIG_STAFF_JWKS_URL — PHP Admin Auth's JWKS
	StaffIssuer   string // CONFIG_STAFF_ISSUER
	StaffAudience string // CONFIG_STAFF_AUDIENCE

	JWKSRefresh     time.Duration // CONFIG_STAFF_JWKS_REFRESH
	JWTClockSkew    time.Duration // CONFIG_JWT_CLOCK_SKEW
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
	LogLevel        slog.Level
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
		ListenAddr:      stringVar("CONFIG_LISTEN_ADDR", ":8080"),
		MetricsAddr:     stringVar("CONFIG_METRICS_ADDR", ":9090"),
		DatabaseURL:     stringVar("CONFIG_DATABASE_URL", ""),
		BlobRoot:        stringVar("CONFIG_BLOB_ROOT", "/var/lib/otomo/blobs"),
		StaffJWKSURL:    stringVar("CONFIG_STAFF_JWKS_URL", ""),
		StaffIssuer:     stringVar("CONFIG_STAFF_ISSUER", ""),
		StaffAudience:   stringVar("CONFIG_STAFF_AUDIENCE", ""),
		DBMaxConns:      intVar("CONFIG_DB_MAX_CONNS", 8, &problems),
		MaxPackBytes:    int64Var("CONFIG_MAX_PACK_BYTES", 536870912, &problems),
		JWKSRefresh:     durVar("CONFIG_STAFF_JWKS_REFRESH", 30*time.Second, &problems),
		JWTClockSkew:    skewVar("CONFIG_JWT_CLOCK_SKEW", 30*time.Second, &problems),
		ReadTimeout:     durVar("CONFIG_READ_TIMEOUT", 10*time.Second, &problems),
		WriteTimeout:    durVar("CONFIG_WRITE_TIMEOUT", 30*time.Second, &problems),
		IdleTimeout:     durVar("CONFIG_IDLE_TIMEOUT", 120*time.Second, &problems),
		ShutdownTimeout: durVar("CONFIG_SHUTDOWN_TIMEOUT", 15*time.Second, &problems),
		LogLevel:        levelVar("CONFIG_LOG_LEVEL", slog.LevelInfo, &problems),
	}

	if cfg.DatabaseURL == "" {
		problems = append(problems, "CONFIG_DATABASE_URL is required")
	}
	if mode == ModeServe {
		if cfg.StaffJWKSURL == "" {
			problems = append(problems, "CONFIG_STAFF_JWKS_URL is required")
		}
		if cfg.StaffIssuer == "" {
			problems = append(problems, "CONFIG_STAFF_ISSUER is required")
		}
		if cfg.StaffAudience == "" {
			problems = append(problems, "CONFIG_STAFF_AUDIENCE is required")
		}
		if cfg.BlobRoot == "" {
			problems = append(problems, "CONFIG_BLOB_ROOT is required")
		}
	}
	if cfg.StaffJWKSURL != "" {
		// A JWKS URL with a typo in it is indistinguishable from PHP Admin Auth being
		// down once the process is running: both leave the service 401ing every staff
		// request. Checking the shape here turns that into a start-up error.
		if u, err := url.Parse(cfg.StaffJWKSURL); err != nil || u.Scheme == "" || u.Host == "" {
			problems = append(problems, fmt.Sprintf("CONFIG_STAFF_JWKS_URL %q is not an absolute URL", cfg.StaffJWKSURL))
		}
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

// int64Var parses key as a positive int64, recording a problem and returning def when
// it is not. It is intVar for byte counts, which are larger than a connection count
// can be. Zero and negative values are rejected for the same reason.
func int64Var(key string, def int64, problems *[]string) int64 {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		*problems = append(*problems, fmt.Sprintf("%s %q is not a positive integer", key, raw))
		return def
	}
	return n
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

// skewVar is durVar for CONFIG_JWT_CLOCK_SKEW, with the one difference that zero is
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
