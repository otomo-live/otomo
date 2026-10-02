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
)

// Mode names the command a configuration is being loaded for. It matters because the
// commands need different things: only serve needs a signing key to sign with.
type Mode string

const (
	ModeServe   Mode = "serve"   // long-running HTTP service
	ModeMigrate Mode = "migrate" // one-shot schema migration
	ModeGenkey  Mode = "genkey"  // one-shot signing-key generation
)

// Config is the fully resolved configuration for one command. Every field is filled
// in by Load, from its environment variable or from that variable's default; no field
// is left for a caller to supply.
type Config struct {
	ListenAddr      string // AUTH_LISTEN_ADDR — the public listener
	MetricsAddr     string // AUTH_METRICS_ADDR — health, metrics and pprof
	DatabaseURL     string // AUTH_DATABASE_URL
	DBMaxConns      int32  // AUTH_DB_MAX_CONNS
	SigningKeyPath  string // AUTH_SIGNING_KEY_PATH — a PKCS#8 PEM private key
	Issuer          string // AUTH_ISSUER — the iss claim on every token issued
	Audience        string // AUTH_AUDIENCE — the aud claim on every token issued
	AccessTokenTTL  time.Duration
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
	LogLevel        slog.Level

	RefreshTokenTTL time.Duration // AUTH_REFRESH_TOKEN_TTL — 30 days, sliding
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
		ListenAddr:      stringVar("AUTH_LISTEN_ADDR", ":8080"),
		MetricsAddr:     stringVar("AUTH_METRICS_ADDR", ":9090"),
		DatabaseURL:     stringVar("AUTH_DATABASE_URL", ""),
		SigningKeyPath:  stringVar("AUTH_SIGNING_KEY_PATH", ""),
		Issuer:          stringVar("AUTH_ISSUER", "https://auth.otomo.internal"),
		Audience:        stringVar("AUTH_AUDIENCE", "otomo:player"),
		DBMaxConns:      intVar("AUTH_DB_MAX_CONNS", 8, &problems),
		AccessTokenTTL:  durVar("AUTH_ACCESS_TOKEN_TTL", 15*time.Minute, &problems),
		RefreshTokenTTL: durVar("AUTH_REFRESH_TOKEN_TTL", 720*time.Hour, &problems),
		ReadTimeout:     durVar("AUTH_READ_TIMEOUT", 10*time.Second, &problems),
		WriteTimeout:    durVar("AUTH_WRITE_TIMEOUT", 30*time.Second, &problems),
		IdleTimeout:     durVar("AUTH_IDLE_TIMEOUT", 120*time.Second, &problems),
		ShutdownTimeout: durVar("AUTH_SHUTDOWN_TIMEOUT", 15*time.Second, &problems),
		LogLevel:        levelVar("AUTH_LOG_LEVEL", slog.LevelInfo, &problems),
	}

	if cfg.DatabaseURL == "" {
		problems = append(problems, "AUTH_DATABASE_URL is required")
	}
	if cfg.SigningKeyPath == "" && mode == ModeServe {
		problems = append(problems, "AUTH_SIGNING_KEY_PATH is required")
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
