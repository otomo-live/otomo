// Package config reads and validates the Allocator service's configuration from the
// environment.
//
// Load runs once at start-up and reports every problem at once rather than stopping at
// the first, so a single start-up tells an operator everything wrong with a
// container's environment. Durations are Go duration strings ("30s", "15m") and the
// log level is a slog name (debug, info, warn, error). LoadDatabase is the same idea
// for the one-shot migrate command, which needs only the database URL.
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
// The Allocator is internal-only: it has no public route, so there is no gateway
// identity or CORS configuration here. What it does have is key material — a signing
// key for the short-lived tickets it mints, and one shared secret per caller it
// accepts — and the address game servers are told to connect to.
type Config struct {
	DatabaseURL string // ALLOCATOR_DATABASE_URL — the allocator role
	ListenAddr  string // ALLOCATOR_LISTEN_ADDR — the internal API listener
	MetricsAddr string // ALLOCATOR_METRICS_ADDR — health, metrics and pprof
	LogLevel    slog.Level
	DBMaxConns  int32 // ALLOCATOR_DB_MAX_CONNS

	// SigningKeyPath is a PKCS#8 PEM private key, the same shape Auth uses. The
	// four *_KEY_PATH variables below point at files holding one base64url service
	// key each: SessionKeyPath is Session's key for asking for a server,
	// GameServerKeyPath is the key every game server registers and heartbeats with,
	// CallbackKeyPath is the key the Allocator presents when it calls Session back
	// (design decision D4), and ProxyKeyPath is the Gameplay Proxy's key for reading
	// where the server named by a ticket lives. They are separate files so a leak of
	// one shared secret does not have to be rotated in both directions.
	SigningKeyPath    string // ALLOCATOR_SIGNING_KEY_PATH
	SessionKeyPath    string // ALLOCATOR_SESSION_KEY_PATH
	GameServerKeyPath string // ALLOCATOR_GAMESERVER_KEY_PATH
	CallbackKeyPath   string // ALLOCATOR_CALLBACK_KEY_PATH
	ProxyKeyPath      string // ALLOCATOR_PROXY_KEY_PATH

	// SessionURL is Session's Compose address, used for callback traffic. It is
	// never handed to a client, so unlike Auth's public URL it is a service name
	// rather than a Gateway address.
	SessionURL string // ALLOCATOR_SESSION_URL

	// PublicAddress and PublicPort are what a game server is told to hand a player:
	// the Gameplay Proxy's dialable host and port, not the Allocator's own listener.
	PublicAddress string // ALLOCATOR_PUBLIC_ADDRESS
	PublicPort    int    // ALLOCATOR_PUBLIC_PORT

	// Issuer and Audience are the iss and aud claims on every ticket the Allocator
	// signs. Both are checked by whoever validates the ticket, so they are configured
	// rather than hardcoded.
	Issuer   string // ALLOCATOR_ISSUER
	Audience string // ALLOCATOR_AUDIENCE

	// TicketTTL bounds how long a minted ticket is valid; ReservationTTL bounds how
	// long a server is held for a party that has not connected yet. Both are kept
	// short on purpose — a reservation that outlives its party is a server nobody
	// else can use.
	TicketTTL      time.Duration // ALLOCATOR_TICKET_TTL
	ReservationTTL time.Duration // ALLOCATOR_RESERVATION_TTL

	// HeartbeatTimeout is how long a game server may go without a heartbeat before
	// it is considered gone, and ReapInterval is how often the reaper looks for such
	// servers. ReapInterval must be shorter than HeartbeatTimeout, or a server could
	// be reaped before its next heartbeat was ever due.
	HeartbeatTimeout time.Duration // ALLOCATOR_HEARTBEAT_TIMEOUT
	ReapInterval     time.Duration // ALLOCATOR_REAP_INTERVAL

	ReadTimeout     time.Duration // ALLOCATOR_READ_TIMEOUT
	WriteTimeout    time.Duration // ALLOCATOR_WRITE_TIMEOUT
	IdleTimeout     time.Duration // ALLOCATOR_IDLE_TIMEOUT
	ShutdownTimeout time.Duration // ALLOCATOR_SHUTDOWN_TIMEOUT
}

// Load builds the Config, reading each variable through the helpers below.
//
// A malformed or out-of-range value is not fatal on its own: its default is used and
// the problem recorded, so the returned error names every variable that needs fixing
// at once instead of one per restart. Missing required variables are collected the
// same way.
func Load() (Config, error) {
	var problems []string

	cfg := Config{
		DatabaseURL:       stringVar("ALLOCATOR_DATABASE_URL", ""),
		ListenAddr:        stringVar("ALLOCATOR_LISTEN_ADDR", ":8080"),
		MetricsAddr:       stringVar("ALLOCATOR_METRICS_ADDR", ":9090"),
		LogLevel:          levelVar("ALLOCATOR_LOG_LEVEL", slog.LevelInfo, &problems),
		DBMaxConns:        int32(intVar("ALLOCATOR_DB_MAX_CONNS", 8, 1, 100, &problems)),
		SigningKeyPath:    stringVar("ALLOCATOR_SIGNING_KEY_PATH", "/run/secrets/allocator/signing_key.pem"),
		SessionKeyPath:    stringVar("ALLOCATOR_SESSION_KEY_PATH", "/run/secrets/allocator_session.key"),
		GameServerKeyPath: stringVar("ALLOCATOR_GAMESERVER_KEY_PATH", "/run/secrets/allocator_gameserver.key"),
		CallbackKeyPath:   stringVar("ALLOCATOR_CALLBACK_KEY_PATH", "/run/secrets/session_allocator.key"),
		ProxyKeyPath:      stringVar("ALLOCATOR_PROXY_KEY_PATH", "/run/secrets/allocator_proxy.key"),
		SessionURL:        stringVar("ALLOCATOR_SESSION_URL", "http://session:8081"),
		PublicAddress:     stringVar("ALLOCATOR_PUBLIC_ADDRESS", ""),
		PublicPort:        intVar("ALLOCATOR_PUBLIC_PORT", 27000, 1, 65535, &problems),
		Issuer:            stringVar("ALLOCATOR_ISSUER", "https://allocator.otomo.internal"),
		Audience:          stringVar("ALLOCATOR_AUDIENCE", "otomo:gameserver"),
		TicketTTL:         durRangeVar("ALLOCATOR_TICKET_TTL", 60*time.Second, 10*time.Second, 10*time.Minute, &problems),
		ReservationTTL:    durRangeVar("ALLOCATOR_RESERVATION_TTL", 60*time.Second, 10*time.Second, 10*time.Minute, &problems),
		HeartbeatTimeout:  durVar("ALLOCATOR_HEARTBEAT_TIMEOUT", 15*time.Second, &problems),
		ReapInterval:      durVar("ALLOCATOR_REAP_INTERVAL", 5*time.Second, &problems),
		ReadTimeout:       durVar("ALLOCATOR_READ_TIMEOUT", 10*time.Second, &problems),
		WriteTimeout:      durVar("ALLOCATOR_WRITE_TIMEOUT", 10*time.Second, &problems),
		IdleTimeout:       durVar("ALLOCATOR_IDLE_TIMEOUT", 60*time.Second, &problems),
		ShutdownTimeout:   durVar("ALLOCATOR_SHUTDOWN_TIMEOUT", 15*time.Second, &problems),
	}

	// The two variables with no default are the only ones an operator must set. They
	// are reported together so one restart fixes both.
	if cfg.DatabaseURL == "" {
		problems = append(problems, "ALLOCATOR_DATABASE_URL is required")
	}
	if cfg.PublicAddress == "" {
		problems = append(problems, "ALLOCATOR_PUBLIC_ADDRESS is required")
	}

	// The defaults above keep these non-empty; the checks state the requirement
	// explicitly so a future default of "" cannot quietly turn a missing path into a
	// start-up with no key to load.
	for _, c := range []struct {
		key, value string
	}{
		{"ALLOCATOR_SIGNING_KEY_PATH", cfg.SigningKeyPath},
		{"ALLOCATOR_SESSION_KEY_PATH", cfg.SessionKeyPath},
		{"ALLOCATOR_GAMESERVER_KEY_PATH", cfg.GameServerKeyPath},
		{"ALLOCATOR_CALLBACK_KEY_PATH", cfg.CallbackKeyPath},
		{"ALLOCATOR_PROXY_KEY_PATH", cfg.ProxyKeyPath},
		{"ALLOCATOR_ISSUER", cfg.Issuer},
		{"ALLOCATOR_AUDIENCE", cfg.Audience},
	} {
		if c.value == "" {
			problems = append(problems, fmt.Sprintf("%s is required", c.key))
		}
	}

	// A callback URL with a typo in it is indistinguishable from Session being down
	// once the process is running. Checking the shape here turns that into a start-up
	// error.
	if u, err := url.Parse(cfg.SessionURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		problems = append(problems, fmt.Sprintf("ALLOCATOR_SESSION_URL %q is not an absolute http(s) URL", cfg.SessionURL))
	}

	// The reaper must run more often than a server is allowed to miss heartbeats,
	// otherwise a server whose next heartbeat has not come due yet can be reaped.
	if cfg.ReapInterval >= cfg.HeartbeatTimeout {
		problems = append(problems, fmt.Sprintf("ALLOCATOR_REAP_INTERVAL %s must be shorter than ALLOCATOR_HEARTBEAT_TIMEOUT %s", cfg.ReapInterval, cfg.HeartbeatTimeout))
	}

	if len(problems) > 0 {
		return Config{}, fmt.Errorf("invalid configuration: %s", strings.Join(problems, "; "))
	}
	return cfg, nil
}

// LoadDatabase builds the subset of Config that a one-shot database command needs:
// migrate opens a connection and applies the embedded schema, and that has nothing to
// do with serving traffic.
//
// It exists separately from Load because Load deliberately requires
// ALLOCATOR_PUBLIC_ADDRESS and validates the key paths, and an operator running a
// migration on a fresh host — before the secrets are mounted and before the public
// address is known — must not have to fake those values. Only the database URL is
// required; the log level has a default, so migrate still logs at the level the
// operator chose.
func LoadDatabase() (Config, error) {
	var problems []string

	cfg := Config{
		DatabaseURL: stringVar("ALLOCATOR_DATABASE_URL", ""),
		LogLevel:    levelVar("ALLOCATOR_LOG_LEVEL", slog.LevelInfo, &problems),
	}
	if cfg.DatabaseURL == "" {
		problems = append(problems, "ALLOCATOR_DATABASE_URL is required")
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
// because a connection count or a port outside its range is a configuration error, not
// a value to clamp.
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
// passed to net/http, where it would mean "no timeout" rather than "instant".
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

// durRangeVar is durVar for the two TTLs, which have a floor as well as a ceiling: a
// ticket valid for less than the round trip that redeems it, or a reservation that
// outlives the party it is held for, are both bugs this catches at start-up.
func durRangeVar(key string, def, min, max time.Duration, problems *[]string) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		*problems = append(*problems, fmt.Sprintf("%s %q is not a duration", key, raw))
		return def
	}
	if d < min || d > max {
		*problems = append(*problems, fmt.Sprintf("%s %q must be between %s and %s", key, raw, min, max))
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
