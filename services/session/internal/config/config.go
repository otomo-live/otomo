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
// commands need different things: only serve needs Valkey and the two identity
// domains, and only serve holds a long-poll open.
type Mode string

const (
	ModeServe   Mode = "serve"   // long-running HTTP service
	ModeMigrate Mode = "migrate" // one-shot schema migration
)

// Config is the fully resolved configuration for one command. Every field is filled
// in by Load, from its environment variable or from that variable's default; no field
// is left for a caller to supply.
//
// There is deliberately no field for a signing key. Session verifies tokens against
// two JWKS endpoints and never mints one: player tokens come from Auth, staff tokens
// from PHP Admin Auth, and the private halves exist only inside those two.
type Config struct {
	ListenAddr  string // SESSION_LISTEN_ADDR — the public listener
	MetricsAddr string // SESSION_METRICS_ADDR — health, metrics and pprof

	// InternalAddr is the listener for the Allocator's callback (LB-4), and
	// CallbackKeyPath the D4 key it presents there (session_allocator.key). No gateway
	// points at this listener. With no key path, every call to it answers 401 and only
	// the repair poll returns a party from its match.
	InternalAddr    string // SESSION_INTERNAL_ADDR
	CallbackKeyPath string // SESSION_CALLBACK_KEY_PATH

	DatabaseURL string // SESSION_DATABASE_URL
	DBMaxConns  int32

	// ValkeyURL points at the store holding presence, pending events and the pub/sub
	// channel that wakes a long-poll. It is a URL rather than host/port fields so the
	// deployed password and ACL username travel with it, and so a test can point at a
	// unix socket without a second code path.
	ValkeyURL string // SESSION_VALKEY_URL

	// The player identity domain: Auth's JWKS, issuer and audience. Player routes
	// accept nothing else, and a staff token presented on one is rejected — the two
	// domains are separate keys, separate issuers and separate audiences (SES-A1).
	PlayerJWKSURL  string // SESSION_PLAYER_JWKS_URL
	PlayerIssuer   string // SESSION_PLAYER_ISSUER
	PlayerAudience string // SESSION_PLAYER_AUDIENCE

	// The staff identity domain: PHP Admin Auth's JWKS. Only the small admin surface
	// under /api/admin/session is reachable with these.
	StaffJWKSURL  string // SESSION_STAFF_JWKS_URL
	StaffIssuer   string // SESSION_STAFF_ISSUER
	StaffAudience string // SESSION_STAFF_AUDIENCE

	JWKSRefresh  time.Duration // SESSION_JWKS_REFRESH — shared by both domains
	JWTClockSkew time.Duration // SESSION_JWT_CLOCK_SKEW

	// EventHold is how long GET /events keeps a request open before answering [].
	// §4 specifies 25 s and §5a Session-E4 gives Gateway a 35 s read timeout on that
	// route; the two are checked against each other at startup rather than left as a
	// comment, because a hold longer than the proxy's timeout turns every idle poll
	// into a 504.
	EventHold time.Duration // SESSION_EVENT_HOLD

	// The rules loader (LB-1): where Patch serves the server manifest on its internal
	// listener, the D4 key Session presents there (patch_session.key), which channel's
	// rules to follow, and how often to ask. With no key path the loader is off and the
	// compiled-in defaults apply.
	PatchURL     string        // SESSION_PATCH_URL
	PatchKeyPath string        // SESSION_PATCH_KEY_PATH
	RulesChannel string        // SESSION_RULES_CHANNEL
	RulesPoll    time.Duration // SESSION_RULES_POLL

	// The Allocator (LB-3): where lobby launches ask for a game server, and the D4 key
	// Session presents there (allocator_session.key). With no key path, launching
	// answers 503 launch_unavailable.
	AllocatorURL     string // SESSION_ALLOCATOR_URL
	AllocatorKeyPath string // SESSION_ALLOCATOR_KEY_PATH

	// RequireReleaseHeader refuses a player request without X-Otomo-Release (SE-8, D5).
	// Off until the SDK sends the header: a request without it is let
	// through, and one with a release other than the live head is refused either way.
	RequireReleaseHeader bool // SESSION_REQUIRE_RELEASE_HEADER

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
		ListenAddr:           stringVar("SESSION_LISTEN_ADDR", ":8080"),
		MetricsAddr:          stringVar("SESSION_METRICS_ADDR", ":9090"),
		InternalAddr:         stringVar("SESSION_INTERNAL_ADDR", ":8081"),
		CallbackKeyPath:      stringVar("SESSION_CALLBACK_KEY_PATH", ""),
		DatabaseURL:          stringVar("SESSION_DATABASE_URL", ""),
		ValkeyURL:            stringVar("SESSION_VALKEY_URL", ""),
		PlayerJWKSURL:        stringVar("SESSION_PLAYER_JWKS_URL", ""),
		PlayerIssuer:         stringVar("SESSION_PLAYER_ISSUER", ""),
		PlayerAudience:       stringVar("SESSION_PLAYER_AUDIENCE", ""),
		StaffJWKSURL:         stringVar("SESSION_STAFF_JWKS_URL", ""),
		StaffIssuer:          stringVar("SESSION_STAFF_ISSUER", ""),
		StaffAudience:        stringVar("SESSION_STAFF_AUDIENCE", ""),
		DBMaxConns:           intVar("SESSION_DB_MAX_CONNS", 8, &problems),
		JWKSRefresh:          durVar("SESSION_JWKS_REFRESH", 30*time.Second, &problems),
		JWTClockSkew:         skewVar("SESSION_JWT_CLOCK_SKEW", 30*time.Second, &problems),
		EventHold:            durVar("SESSION_EVENT_HOLD", 25*time.Second, &problems),
		PatchURL:             stringVar("SESSION_PATCH_URL", "http://patch:8081"),
		PatchKeyPath:         stringVar("SESSION_PATCH_KEY_PATH", ""),
		RulesChannel:         stringVar("SESSION_RULES_CHANNEL", "live"),
		RulesPoll:            durVar("SESSION_RULES_POLL", 60*time.Second, &problems),
		AllocatorURL:         stringVar("SESSION_ALLOCATOR_URL", "http://allocator:8080"),
		AllocatorKeyPath:     stringVar("SESSION_ALLOCATOR_KEY_PATH", ""),
		RequireReleaseHeader: boolVar("SESSION_REQUIRE_RELEASE_HEADER", false, &problems),
		ReadTimeout:          durVar("SESSION_READ_TIMEOUT", 35*time.Second, &problems),
		WriteTimeout:         durVar("SESSION_WRITE_TIMEOUT", 40*time.Second, &problems),
		IdleTimeout:          durVar("SESSION_IDLE_TIMEOUT", 120*time.Second, &problems),
		ShutdownTimeout:      durVar("SESSION_SHUTDOWN_TIMEOUT", 15*time.Second, &problems),
		LogLevel:             levelVar("SESSION_LOG_LEVEL", slog.LevelInfo, &problems),
	}

	if cfg.DatabaseURL == "" {
		problems = append(problems, "SESSION_DATABASE_URL is required")
	}
	if mode == ModeServe {
		required := []struct{ key, value string }{
			{"SESSION_VALKEY_URL", cfg.ValkeyURL},
			{"SESSION_PLAYER_JWKS_URL", cfg.PlayerJWKSURL},
			{"SESSION_PLAYER_ISSUER", cfg.PlayerIssuer},
			{"SESSION_PLAYER_AUDIENCE", cfg.PlayerAudience},
			{"SESSION_STAFF_JWKS_URL", cfg.StaffJWKSURL},
			{"SESSION_STAFF_ISSUER", cfg.StaffIssuer},
			{"SESSION_STAFF_AUDIENCE", cfg.StaffAudience},
		}
		for _, r := range required {
			if r.value == "" {
				problems = append(problems, r.key+" is required")
			}
		}

		// Two JWKS URLs with a typo in them are indistinguishable from the issuers
		// being down once the process is running: both leave the service rejecting
		// every request. Checking the shape here turns that into a start-up error.
		for _, u := range []struct{ key, value string }{
			{"SESSION_PLAYER_JWKS_URL", cfg.PlayerJWKSURL},
			{"SESSION_STAFF_JWKS_URL", cfg.StaffJWKSURL},
		} {
			if u.value == "" {
				continue
			}
			if parsed, err := url.Parse(u.value); err != nil || parsed.Scheme == "" || parsed.Host == "" {
				problems = append(problems, fmt.Sprintf("%s %q is not an absolute URL", u.key, u.value))
			}
		}

		// The two domains must not be the same domain. A copy-paste that leaves both
		// pointing at one JWKS would make every staff token a valid player token, and
		// the audience check would not catch it because both claims would then be
		// checked against the same issuer's set — each would simply fail its own
		// audience and the service would look broken rather than unsafe. Comparing the
		// issuers is the check that catches the mistake at boot.
		if cfg.PlayerIssuer != "" && cfg.PlayerIssuer == cfg.StaffIssuer {
			problems = append(problems,
				"SESSION_PLAYER_ISSUER and SESSION_STAFF_ISSUER are the same issuer; player and staff tokens must be distinguishable")
		}

		if parsed, err := url.Parse(cfg.PatchURL); err != nil || parsed.Scheme == "" || parsed.Host == "" {
			problems = append(problems, fmt.Sprintf("SESSION_PATCH_URL %q is not an absolute URL", cfg.PatchURL))
		}
		if parsed, err := url.Parse(cfg.AllocatorURL); err != nil || parsed.Scheme == "" || parsed.Host == "" {
			problems = append(problems, fmt.Sprintf("SESSION_ALLOCATOR_URL %q is not an absolute URL", cfg.AllocatorURL))
		}
		// The callback listener must be its own port: on the public one a gateway would
		// route to it, and on the metrics one it would sit beside unauthenticated pprof.
		if cfg.InternalAddr == cfg.ListenAddr || cfg.InternalAddr == cfg.MetricsAddr {
			problems = append(problems, fmt.Sprintf(
				"SESSION_INTERNAL_ADDR %q must differ from SESSION_LISTEN_ADDR and SESSION_METRICS_ADDR", cfg.InternalAddr))
		}
		switch cfg.RulesChannel {
		case "dev", "staging", "live":
		default:
			problems = append(problems, fmt.Sprintf("SESSION_RULES_CHANNEL %q is not dev, staging or live", cfg.RulesChannel))
		}

		// The hold has to fit inside the proxy's read timeout with room for the
		// response. §5a sets Gateway to 35 s; leaving less than a second of headroom
		// means an idle poll is answered by the proxy's timeout instead of by us.
		if cfg.EventHold >= cfg.ReadTimeout {
			problems = append(problems, fmt.Sprintf(
				"SESSION_EVENT_HOLD (%s) must be shorter than SESSION_READ_TIMEOUT (%s), which must in turn stay under Gateway's read timeout for this route",
				cfg.EventHold, cfg.ReadTimeout))
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

// boolVar parses key as true or false (strconv.ParseBool's spellings), recording a
// problem and returning def when it is neither.
func boolVar(key string, def bool, problems *[]string) bool {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		*problems = append(*problems, fmt.Sprintf("%s %q is not true or false", key, raw))
		return def
	}
	return b
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

// skewVar is durVar for SESSION_JWT_CLOCK_SKEW, with the one difference that zero is
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
