// Package config reads and validates the Gameplay Proxy's configuration from the
// environment.
//
// Load runs once at start-up and reports every problem at once rather than stopping at
// the first, so a single start-up tells an operator everything wrong with a container's
// environment. Durations are Go duration strings ("30s", "15m") and the log level is a
// slog name (debug, info, warn, error).
//
// Nothing here reads a file. The Allocator key is a path, and whether it is readable
// and well formed is checked in main; that keeps Load a pure function of the
// environment and lets every configuration test run without a secrets mount.
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
type Config struct {
	// ListenAddr is the public UDP listener players send handshakes and game traffic
	// to. MetricsAddr is the private HTTP listener for health and Prometheus; like the
	// Allocator's it has no authentication of its own, so it must never be exposed
	// outside the Docker network.
	ListenAddr  string // PROXY_LISTEN_ADDR
	MetricsAddr string // PROXY_METRICS_ADDR

	// AllocatorURL is the Allocator's Compose address. The proxy fetches both the
	// ticket JWKS and the server directory from it, over the internal network only.
	AllocatorURL string // PROXY_ALLOCATOR_URL

	// AllocatorKeyPath points at the base64url key the proxy presents as a bearer
	// token when it polls /internal/servers. It is a path, not the key, so the secret
	// never enters the process environment.
	AllocatorKeyPath string // PROXY_ALLOCATOR_KEY_PATH

	// Issuer and Audience are the iss and aud the proxy requires on a ticket. They
	// have the same defaults as the Allocator's signing configuration, so an
	// unconfigured pair still matches in a normal deployment.
	Issuer   string // PROXY_TICKET_ISSUER
	Audience string // PROXY_TICKET_AUDIENCE

	// IdleTimeout is how long a client-to-server session may go without a datagram in
	// either direction before the proxy forgets it. DirectoryRefresh is how often the
	// proxy polls the Allocator for the server table.
	IdleTimeout      time.Duration // PROXY_IDLE_TIMEOUT
	DirectoryRefresh time.Duration // PROXY_DIRECTORY_REFRESH

	// MaxSessions bounds the number of live client sessions. A handshake that would
	// exceed it is refused rather than allowed to grow memory without limit.
	MaxSessions int // PROXY_MAX_SESSIONS

	// ShutdownTimeout bounds the private HTTP listener's graceful drain on SIGTERM.
	ShutdownTimeout time.Duration // PROXY_SHUTDOWN_TIMEOUT

	LogLevel slog.Level // PROXY_LOG_LEVEL
}

// Load builds the Config, reading each variable through the helpers below.
//
// A malformed or out-of-range value is not fatal on its own: its default is used and
// the problem recorded, so the returned error names every variable that needs fixing at
// once instead of one per restart.
func Load() (Config, error) {
	var problems []string

	cfg := Config{
		ListenAddr:       stringVar("PROXY_LISTEN_ADDR", ":27000"),
		MetricsAddr:      stringVar("PROXY_METRICS_ADDR", ":9090"),
		AllocatorURL:     stringVar("PROXY_ALLOCATOR_URL", "http://allocator:8080"),
		AllocatorKeyPath: stringVar("PROXY_ALLOCATOR_KEY_PATH", "/run/secrets/allocator_proxy.key"),
		Issuer:           stringVar("PROXY_TICKET_ISSUER", "https://allocator.otomo.internal"),
		Audience:         stringVar("PROXY_TICKET_AUDIENCE", "otomo:gameserver"),
		IdleTimeout:      durVar("PROXY_IDLE_TIMEOUT", 30*time.Second, &problems),
		DirectoryRefresh: durVar("PROXY_DIRECTORY_REFRESH", 5*time.Second, &problems),
		MaxSessions:      intVar("PROXY_MAX_SESSIONS", 2000, 1, 1_000_000, &problems),
		ShutdownTimeout:  durVar("PROXY_SHUTDOWN_TIMEOUT", 15*time.Second, &problems),
		LogLevel:         levelVar("PROXY_LOG_LEVEL", slog.LevelInfo, &problems),
	}

	// The Allocator URL is the proxy's only upstream: every ticket key and every
	// server address comes from it. A typo in the scheme or host is indistinguishable
	// from the Allocator being down once the process is running, so it is checked for
	// shape here.
	if u, err := url.Parse(cfg.AllocatorURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		problems = append(problems, fmt.Sprintf("PROXY_ALLOCATOR_URL %q is not an absolute http(s) URL", cfg.AllocatorURL))
	}

	// The defaults above keep these non-empty; the checks state the requirement
	// explicitly so a future default of "" cannot quietly turn a missing value into a
	// listener that binds everywhere or a request with no Authorization header.
	for _, c := range []struct {
		key, value string
	}{
		{"PROXY_LISTEN_ADDR", cfg.ListenAddr},
		{"PROXY_METRICS_ADDR", cfg.MetricsAddr},
		{"PROXY_ALLOCATOR_KEY_PATH", cfg.AllocatorKeyPath},
		{"PROXY_TICKET_ISSUER", cfg.Issuer},
		{"PROXY_TICKET_AUDIENCE", cfg.Audience},
	} {
		if c.value == "" {
			problems = append(problems, fmt.Sprintf("%s is required", c.key))
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

// intVar parses key as an integer within [min, max], recording a problem and returning
// def when it is not. The bounds are checked here rather than left to the caller
// because a session limit outside its range is a configuration error, not a value to
// clamp.
func intVar(key string, def, min, max int, problems *[]string) int {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		*problems = append(*problems, fmt.Sprintf("%s %q is not an integer", key, raw))
		return def
	}
	if n < min || n > max {
		*problems = append(*problems, fmt.Sprintf("%s %q must be between %d and %d", key, raw, min, max))
		return def
	}
	return n
}

// durVar parses key as a positive Go duration ("30s", "15m"), recording a problem and
// returning def when it is not. A zero or negative timeout is rejected rather than
// passed to the idle reaper, where it would mean "expire immediately" rather than "no
// timeout".
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

// levelVar parses key as a slog level name, recording a problem and returning def when
// it is not. Go's own UnmarshalText defines the accepted spellings, so this accepts
// exactly what slog accepts and nothing extra.
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
