package config

import (
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
)

// setAll clears every variable Load reads, so a test sees only what it sets and does not
// inherit a developer's shell. t.Setenv restores the previous values afterwards and marks
// the test as one that may not run in parallel, which is correct here: Load reads the
// process environment.
func setAll(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"DASHBOARD_LISTEN_ADDR", "DASHBOARD_METRICS_ADDR", "DASHBOARD_STAFF_JWKS_URL",
		"DASHBOARD_STAFF_ISSUER", "DASHBOARD_STAFF_AUDIENCE", "DASHBOARD_STAFF_JWKS_REFRESH",
		"DASHBOARD_JWT_CLOCK_SKEW", "DASHBOARD_PROMETHEUS_URL", "DASHBOARD_LOKI_URL",
		"DASHBOARD_UPSTREAM_TIMEOUT", "DASHBOARD_READ_TIMEOUT", "DASHBOARD_WRITE_TIMEOUT",
		"DASHBOARD_IDLE_TIMEOUT", "DASHBOARD_SHUTDOWN_TIMEOUT", "DASHBOARD_LOG_LEVEL",
		"DASHBOARD_TARGETS", "DASHBOARD_PROBE_INTERVAL",
		"DASHBOARD_CONFIG_URL", "DASHBOARD_ADMIN_AUTH_URL", "DASHBOARD_SESSION_URL",
	} {
		t.Setenv(key, "")
	}
}

// serveEnv is the smallest environment that satisfies Load: only the JWKS URL is
// required, because the issuer and audience have defaults.
func serveEnv(t *testing.T) {
	t.Helper()
	setAll(t)
	t.Setenv("DASHBOARD_STAFF_JWKS_URL", "http://php-admin/.well-known/jwks.json")
}

func TestLoadDefaults(t *testing.T) {
	serveEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}

	want := Config{
		ListenAddr:      ":8080",
		MetricsAddr:     ":9090",
		StaffJWKSURL:    "http://php-admin/.well-known/jwks.json",
		StaffIssuer:     "https://admin-auth.otomo.internal",
		StaffAudience:   "otomo:staff",
		JWKSRefresh:     30 * time.Second,
		JWTClockSkew:    30 * time.Second,
		PrometheusURL:   "http://prometheus:9090",
		LokiURL:         "http://loki:3100",
		UpstreamTimeout: 2 * time.Second,
		ConfigURL:       "http://config:8080",
		AdminAuthURL:    "http://admin-auth:8080",
		SessionURL:      "http://session:8080",
		ProbeInterval:   15 * time.Second,
		ReadTimeout:     10 * time.Second,
		WriteTimeout:    30 * time.Second,
		IdleTimeout:     120 * time.Second,
		ShutdownTimeout: 15 * time.Second,
		LogLevel:        slog.LevelInfo,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("Load() = %+v\nwant %+v", cfg, want)
	}
}

// TestLoadRequiresTheStaffJwks keeps the one hard requirement honest: without a JWKS the
// service can never verify a token, so there is no sensible default and refusing to start
// is better than serving a service that 401s everyone with no clue why.
func TestLoadRequiresTheStaffJwks(t *testing.T) {
	setAll(t)

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "DASHBOARD_STAFF_JWKS_URL is required") {
		t.Fatalf("Load() error = %v, want DASHBOARD_STAFF_JWKS_URL is required", err)
	}
}

// TestLoadReportsEveryProblemAtOnce is the property that makes a failed start-up useful:
// a container that reports one problem per restart makes an operator iterate through
// their environment file one line at a time.
func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	serveEnv(t)
	t.Setenv("DASHBOARD_STAFF_JWKS_URL", "not-a-url")
	t.Setenv("DASHBOARD_PROMETHEUS_URL", "prometheus:9090")
	t.Setenv("DASHBOARD_READ_TIMEOUT", "10 seconds")
	t.Setenv("DASHBOARD_SHUTDOWN_TIMEOUT", "-5s")
	t.Setenv("DASHBOARD_JWT_CLOCK_SKEW", "-1s")
	t.Setenv("DASHBOARD_LOG_LEVEL", "verbose")

	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted an environment full of bad values")
	}
	for _, want := range []string{
		"DASHBOARD_STAFF_JWKS_URL",
		"DASHBOARD_PROMETHEUS_URL",
		"DASHBOARD_READ_TIMEOUT",
		"DASHBOARD_SHUTDOWN_TIMEOUT",
		"DASHBOARD_JWT_CLOCK_SKEW",
		"DASHBOARD_LOG_LEVEL",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestLoadRejectsRelativeJwksURL(t *testing.T) {
	tests := []string{
		"/.well-known/jwks.json",
		"php-admin/.well-known/jwks.json",
		"://php-admin",
	}
	for _, url := range tests {
		t.Run(url, func(t *testing.T) {
			serveEnv(t)
			t.Setenv("DASHBOARD_STAFF_JWKS_URL", url)

			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "absolute URL") {
				t.Fatalf("Load() error = %v, want an absolute-URL complaint", err)
			}
		})
	}
}

func TestLoadAcceptsEveryValidValue(t *testing.T) {
	serveEnv(t)
	t.Setenv("DASHBOARD_LISTEN_ADDR", "127.0.0.1:0")
	t.Setenv("DASHBOARD_METRICS_ADDR", "127.0.0.1:0")
	t.Setenv("DASHBOARD_STAFF_ISSUER", "https://php-admin.otomo.internal")
	t.Setenv("DASHBOARD_STAFF_JWKS_REFRESH", "5m")
	t.Setenv("DASHBOARD_JWT_CLOCK_SKEW", "0s")
	t.Setenv("DASHBOARD_PROMETHEUS_URL", "http://prom.internal:9090")
	t.Setenv("DASHBOARD_LOKI_URL", "http://loki.internal:3100")
	t.Setenv("DASHBOARD_UPSTREAM_TIMEOUT", "500ms")
	t.Setenv("DASHBOARD_LOG_LEVEL", "debug")

	// A zero clock skew is the one value that must survive a "positive" check: it is a
	// legitimate choice for a deployment whose containers share a clock source.
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:0" || cfg.PrometheusURL != "http://prom.internal:9090" {
		t.Errorf("Load() = %+v", cfg)
	}
	if cfg.JWKSRefresh != 5*time.Minute || cfg.UpstreamTimeout != 500*time.Millisecond {
		t.Errorf("durations = %s / %s", cfg.JWKSRefresh, cfg.UpstreamTimeout)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %s, want debug", cfg.LogLevel)
	}
	if cfg.StaffIssuer != "https://php-admin.otomo.internal" {
		t.Errorf("StaffIssuer = %q, want the explicit value", cfg.StaffIssuer)
	}
}

// TestLoadTargets covers the DASHBOARD_TARGETS grammar: a good comma-separated list, an
// empty value meaning no targets, and each rejection an operator can hit.
func TestLoadTargets(t *testing.T) {
	t.Run("good list", func(t *testing.T) {
		serveEnv(t)
		t.Setenv("DASHBOARD_TARGETS", "auth=http://auth:9090, config=http://config:9090 ,patch=https://patch:9090,admin-auth=http://admin-auth:9090")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() = %v", err)
		}
		want := []Target{
			{Name: "auth", URL: "http://auth:9090"},
			{Name: "config", URL: "http://config:9090"},
			{Name: "patch", URL: "https://patch:9090"},
			// A compose service name with a hyphen: the same identity Prometheus and
			// Loki label it with.
			{Name: "admin-auth", URL: "http://admin-auth:9090"},
		}
		if !reflect.DeepEqual(cfg.Targets, want) {
			t.Errorf("Targets = %+v, want %+v", cfg.Targets, want)
		}
	})

	t.Run("empty", func(t *testing.T) {
		serveEnv(t)
		t.Setenv("DASHBOARD_TARGETS", "")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() = %v", err)
		}
		if len(cfg.Targets) != 0 {
			t.Errorf("Targets = %+v, want none", cfg.Targets)
		}
	})

	tests := []struct {
		name   string
		value  string
		substr string
	}{
		{"duplicate name", "auth=http://auth:9090,auth=http://auth2:9090", "duplicated"},
		{"uppercase name", "Auth=http://auth:9090", "name"},
		{"leading digit", "1auth=http://auth:9090", "name"},
		{"space in name", "auth x=http://auth:9090", "name"},
		{"quote in name", `auth"=http://auth:9090`, "name"},
		{"too long", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa=http://auth:9090", "name"},
		{"missing equals", "auth", "name=url"},
		{"empty url", "auth=", "name=url"},
		{"relative url", "auth=/readyz", "absolute http(s)"},
		{"schemeless url", "auth=auth:9090", "absolute http(s)"},
		{"ftp url", "auth=ftp://auth/readyz", "absolute http(s)"},
		{"empty entry", "auth=http://auth:9090,", "empty entry"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serveEnv(t)
			t.Setenv("DASHBOARD_TARGETS", tt.value)

			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tt.substr) {
				t.Fatalf("Load() error = %v, want a complaint containing %q", err, tt.substr)
			}
		})
	}
}

// TestLoadProbeInterval pins the floor that keeps the prober from hammering every service
// on the network, and that one second exactly is accepted.
func TestLoadProbeInterval(t *testing.T) {
	t.Run("one second is allowed", func(t *testing.T) {
		serveEnv(t)
		t.Setenv("DASHBOARD_PROBE_INTERVAL", "1s")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() = %v", err)
		}
		if cfg.ProbeInterval != time.Second {
			t.Errorf("ProbeInterval = %s, want 1s", cfg.ProbeInterval)
		}
	})

	for _, value := range []string{"500ms", "0s", "-1s", "abc"} {
		t.Run(value, func(t *testing.T) {
			serveEnv(t)
			t.Setenv("DASHBOARD_PROBE_INTERVAL", value)

			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "DASHBOARD_PROBE_INTERVAL") {
				t.Fatalf("Load() error = %v, want a DASHBOARD_PROBE_INTERVAL complaint", err)
			}
		})
	}
}

// TestLoadTreatsEmptyAsUnset is what stops an empty variable in a compose file from
// blanking out a default and leaving the service with no listener address.
func TestLoadTreatsEmptyAsUnset(t *testing.T) {
	serveEnv(t)
	t.Setenv("DASHBOARD_LISTEN_ADDR", "")
	t.Setenv("DASHBOARD_STAFF_ISSUER", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want the default :8080", cfg.ListenAddr)
	}
	if cfg.StaffIssuer != "https://admin-auth.otomo.internal" {
		t.Errorf("StaffIssuer = %q, want the default", cfg.StaffIssuer)
	}
}

// TestLoadAuditFeedURLs pins the two audit upstreams: their compose defaults, an explicit
// override, and the http(s)-only rule that keeps a typo from becoming a per-request failure.
func TestLoadAuditFeedURLs(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		serveEnv(t)

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() = %v", err)
		}
		if cfg.ConfigURL != "http://config:8080" || cfg.AdminAuthURL != "http://admin-auth:8080" {
			t.Errorf("audit URLs = %q / %q", cfg.ConfigURL, cfg.AdminAuthURL)
		}
	})

	t.Run("override", func(t *testing.T) {
		serveEnv(t)
		t.Setenv("DASHBOARD_CONFIG_URL", "https://config.internal:8443")
		t.Setenv("DASHBOARD_ADMIN_AUTH_URL", "http://admin-auth.internal:8080")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() = %v", err)
		}
		if cfg.ConfigURL != "https://config.internal:8443" || cfg.AdminAuthURL != "http://admin-auth.internal:8080" {
			t.Errorf("audit URLs = %q / %q", cfg.ConfigURL, cfg.AdminAuthURL)
		}
	})

	for _, tt := range []struct {
		key   string
		value string
	}{
		{"DASHBOARD_CONFIG_URL", "config:8080"},
		{"DASHBOARD_CONFIG_URL", "ftp://config:8080"},
		{"DASHBOARD_CONFIG_URL", "/audit"},
		{"DASHBOARD_ADMIN_AUTH_URL", "://admin-auth"},
		{"DASHBOARD_ADMIN_AUTH_URL", "file:///audit"},
	} {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			serveEnv(t)
			t.Setenv(tt.key, tt.value)

			if _, err := Load(); err == nil || !strings.Contains(err.Error(), tt.key) {
				t.Fatalf("Load() error = %v, want a %s complaint", err, tt.key)
			}
		})
	}
}
