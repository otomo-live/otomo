// Package config reads and validates the Dashboard service's configuration from the
// environment.
//
// Load runs once at start-up and reports every problem at once rather than stopping at
// the first, so a single start-up tells an operator everything wrong with a container's
// environment. Durations are Go duration strings ("30s", "15m") and the log level is a
// slog name (debug, info, warn, error).
//
// There is deliberately no field for the staff signing key. Dashboard verifies staff
// tokens against PHP Admin Auth's JWKS and never mints one, so the private key exists
// only inside PHP Admin Auth — the service cannot sign even if it is compromised.
package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Target is one service the health prober polls: a name used in the /services response
// and metric labels, and the internal base URL it appends /readyz to.
type Target struct {
	Name string
	URL  string
}

// targetNameRE matches a compose service name, the same grammar the PromQL catalogue
// accepts for the `service` label (hyphens allowed): the prober's target names, the
// Prometheus `service` label and Loki's `service` label are one identity, so the
// overview lists admin-auth once rather than as both admin-auth and admin_auth.
var targetNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// Config is the fully resolved configuration for the serve command. Every field is
// filled in by Load, from its environment variable or from that variable's default; no
// field is left for a caller to supply.
type Config struct {
	ListenAddr  string // DASHBOARD_LISTEN_ADDR — the public listener
	MetricsAddr string // DASHBOARD_METRICS_ADDR — health, metrics and pprof

	// The staff identity domain from 06-auth-identity-contract.md §3 and §5. Both
	// values are checked against the token's own claims, never inferred from which
	// JWKS answered.
	StaffJWKSURL  string // DASHBOARD_STAFF_JWKS_URL — PHP Admin Auth's JWKS
	StaffIssuer   string // DASHBOARD_STAFF_ISSUER
	StaffAudience string // DASHBOARD_STAFF_AUDIENCE

	JWKSRefresh  time.Duration // DASHBOARD_STAFF_JWKS_REFRESH
	JWTClockSkew time.Duration // DASHBOARD_JWT_CLOCK_SKEW

	// Upstream observability stores. Dashboard is a thin query layer in front of them:
	// it holds no time series or logs of its own. These are not consulted at startup
	// and are not part of readiness — an unreachable store degrades the panels that
	// need it, it does not take the Dashboard out of rotation.
	PrometheusURL   string        // DASHBOARD_PROMETHEUS_URL
	LokiURL         string        // DASHBOARD_LOKI_URL
	UpstreamTimeout time.Duration // DASHBOARD_UPSTREAM_TIMEOUT

	// Audit feed upstreams. The merged GET /audit fans out to these two services'
	// own audit endpoints with the caller's token; like the observability stores they
	// are query-time only and never part of readiness.
	ConfigURL    string // DASHBOARD_CONFIG_URL
	AdminAuthURL string // DASHBOARD_ADMIN_AUTH_URL
	SessionURL   string // DASHBOARD_SESSION_URL (SE-7)

	// Service health prober: the other services whose /readyz this process polls so
	// /services can explain *why* a service is not serving. Not part of readiness —
	// an unreachable target is exactly what the prober is there to report.
	Targets       []Target      // DASHBOARD_TARGETS
	ProbeInterval time.Duration // DASHBOARD_PROBE_INTERVAL

	ReadTimeout     time.Duration // DASHBOARD_READ_TIMEOUT
	WriteTimeout    time.Duration // DASHBOARD_WRITE_TIMEOUT
	IdleTimeout     time.Duration // DASHBOARD_IDLE_TIMEOUT
	ShutdownTimeout time.Duration // DASHBOARD_SHUTDOWN_TIMEOUT
	LogLevel        slog.Level    // DASHBOARD_LOG_LEVEL
}

// Load builds the Config, reading each variable through the helpers below.
//
// A malformed or non-positive value is not fatal on its own: its default is used and the
// problem recorded, so the returned error names every variable that needs fixing at once
// instead of one per restart. The one required variable, the staff JWKS URL, is
// collected the same way.
func Load() (Config, error) {
	var problems []string

	cfg := Config{
		ListenAddr:      stringVar("DASHBOARD_LISTEN_ADDR", ":8080"),
		MetricsAddr:     stringVar("DASHBOARD_METRICS_ADDR", ":9090"),
		StaffJWKSURL:    stringVar("DASHBOARD_STAFF_JWKS_URL", ""),
		StaffIssuer:     stringVar("DASHBOARD_STAFF_ISSUER", "https://admin-auth.otomo.internal"),
		StaffAudience:   stringVar("DASHBOARD_STAFF_AUDIENCE", "otomo:staff"),
		JWKSRefresh:     durVar("DASHBOARD_STAFF_JWKS_REFRESH", 30*time.Second, &problems),
		JWTClockSkew:    skewVar("DASHBOARD_JWT_CLOCK_SKEW", 30*time.Second, &problems),
		PrometheusURL:   stringVar("DASHBOARD_PROMETHEUS_URL", "http://prometheus:9090"),
		LokiURL:         stringVar("DASHBOARD_LOKI_URL", "http://loki:3100"),
		UpstreamTimeout: durVar("DASHBOARD_UPSTREAM_TIMEOUT", 2*time.Second, &problems),
		ConfigURL:       stringVar("DASHBOARD_CONFIG_URL", "http://config:8080"),
		AdminAuthURL:    stringVar("DASHBOARD_ADMIN_AUTH_URL", "http://admin-auth:8080"),
		SessionURL:      stringVar("DASHBOARD_SESSION_URL", "http://session:8080"),
		Targets:         parseTargets(os.Getenv("DASHBOARD_TARGETS"), &problems),
		ProbeInterval:   minDurVar("DASHBOARD_PROBE_INTERVAL", 15*time.Second, time.Second, &problems),
		ReadTimeout:     durVar("DASHBOARD_READ_TIMEOUT", 10*time.Second, &problems),
		WriteTimeout:    durVar("DASHBOARD_WRITE_TIMEOUT", 30*time.Second, &problems),
		IdleTimeout:     durVar("DASHBOARD_IDLE_TIMEOUT", 120*time.Second, &problems),
		ShutdownTimeout: durVar("DASHBOARD_SHUTDOWN_TIMEOUT", 15*time.Second, &problems),
		LogLevel:        levelVar("DASHBOARD_LOG_LEVEL", slog.LevelInfo, &problems),
	}

	if cfg.StaffJWKSURL == "" {
		problems = append(problems, "DASHBOARD_STAFF_JWKS_URL is required")
	}

	// A URL with a typo in it is indistinguishable from the upstream being down once
	// the process is running: the first leaves every staff request 401ing forever, the
	// others leave every panel empty. Checking the shape here turns that into a
	// start-up error naming the variable.
	for _, u := range []struct{ name, value string }{
		{"DASHBOARD_STAFF_JWKS_URL", cfg.StaffJWKSURL},
		{"DASHBOARD_PROMETHEUS_URL", cfg.PrometheusURL},
		{"DASHBOARD_LOKI_URL", cfg.LokiURL},
		{"DASHBOARD_CONFIG_URL", cfg.ConfigURL},
		{"DASHBOARD_ADMIN_AUTH_URL", cfg.AdminAuthURL},
		{"DASHBOARD_SESSION_URL", cfg.SessionURL},
	} {
		if u.value == "" {
			continue
		}
		if parsed, err := url.Parse(u.value); err != nil || parsed.Scheme == "" || parsed.Host == "" {
			problems = append(problems, fmt.Sprintf("%s %q is not an absolute URL", u.name, u.value))
		}
	}

	// The two audit feeds are called by net/http with an added path, so they must be
	// http(s) rather than merely absolute: a `file://` or `ftp://` base URL would parse
	// and then fail on every request.
	for _, u := range []struct{ name, value string }{
		{"DASHBOARD_CONFIG_URL", cfg.ConfigURL},
		{"DASHBOARD_ADMIN_AUTH_URL", cfg.AdminAuthURL},
		{"DASHBOARD_SESSION_URL", cfg.SessionURL},
	} {
		if u.value == "" {
			continue
		}
		parsed, err := url.Parse(u.value)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			problems = append(problems, fmt.Sprintf("%s %q is not an absolute http(s) URL", u.name, u.value))
		}
	}

	if len(problems) > 0 {
		return Config{}, fmt.Errorf("invalid configuration: %s", strings.Join(problems, "; "))
	}
	return cfg, nil
}

// stringVar returns the value of key, or def when it is unset or empty. Treating an
// empty value as unset means an environment variable set to "" cannot silently blank out
// a default.
func stringVar(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// durVar parses key as a positive Go duration ("30s", "15m"), recording a problem and
// returning def when it is not. A zero or negative timeout is rejected rather than passed
// to net/http, where it would mean "no timeout" rather than "instant".
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

// skewVar is durVar for DASHBOARD_JWT_CLOCK_SKEW, with the one difference that zero is
// accepted. It has to be: a deployment whose containers share a clock source has no use
// for leeway, and refusing 0 would force it to state a small nonzero value that means
// something different from what it intends. Timeouts keep the positive-only rule, because
// there 0 means "no timeout" rather than "no leeway".
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

// minDurVar is durVar with a floor the value must reach: DASHBOARD_PROBE_INTERVAL below
// one second would turn an internal /readyz fan-out into a load generator, so a value
// under min is recorded and the default used.
func minDurVar(key string, def, min time.Duration, problems *[]string) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		*problems = append(*problems, fmt.Sprintf("%s %q is not a duration", key, raw))
		return def
	}
	if d < min {
		*problems = append(*problems, fmt.Sprintf("%s %q must be at least %s", key, raw, min))
		return def
	}
	return d
}

// parseTargets parses DASHBOARD_TARGETS: comma-separated name=url pairs where the url is
// a service's internal base URL. An empty value means no targets, which is valid — the
// Dashboard can run before the services it watches exist. Every other malformation is
// recorded rather than silently skipped, because a target that vanishes from the
// overview is worse than a start-up error.
func parseTargets(raw string, problems *[]string) []Target {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	var targets []Target
	seen := make(map[string]bool)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			*problems = append(*problems, "DASHBOARD_TARGETS has an empty entry")
			continue
		}

		name, u, ok := strings.Cut(entry, "=")
		name = strings.TrimSpace(name)
		u = strings.TrimSpace(u)
		if !ok || name == "" || u == "" {
			*problems = append(*problems, fmt.Sprintf("DASHBOARD_TARGETS entry %q is not name=url", entry))
			continue
		}
		if !targetNameRE.MatchString(name) {
			*problems = append(*problems, fmt.Sprintf("DASHBOARD_TARGETS target name %q does not match %s", name, targetNameRE))
			continue
		}
		if seen[name] {
			*problems = append(*problems, fmt.Sprintf("DASHBOARD_TARGETS target name %q is duplicated", name))
			continue
		}
		seen[name] = true

		parsed, err := url.Parse(u)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			*problems = append(*problems, fmt.Sprintf("DASHBOARD_TARGETS target %q url %q is not an absolute http(s) URL", name, u))
			continue
		}
		targets = append(targets, Target{Name: name, URL: u})
	}
	return targets
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
