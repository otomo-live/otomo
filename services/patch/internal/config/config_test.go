package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// setAll clears every variable Load reads, so a test sees only what it sets and does
// not inherit a developer's shell. t.Setenv restores the previous values afterwards
// and marks the test as one that may not run in parallel, which is correct here:
// Load reads the process environment.
func setAll(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"PATCH_LISTEN_ADDR", "PATCH_METRICS_ADDR", "PATCH_DATABASE_URL",
		"PATCH_DB_MAX_CONNS", "PATCH_BLOB_ROOT", "PATCH_STAFF_JWKS_URL",
		"PATCH_STAFF_ISSUER", "PATCH_STAFF_AUDIENCE", "PATCH_STAFF_JWKS_REFRESH",
		"PATCH_JWT_CLOCK_SKEW", "PATCH_POLL_INTERVAL", "PATCH_READ_TIMEOUT",
		"PATCH_WRITE_TIMEOUT", "PATCH_IDLE_TIMEOUT", "PATCH_SHUTDOWN_TIMEOUT",
		"PATCH_LOG_LEVEL",
	} {
		t.Setenv(key, "")
	}
}

// serveEnv is the smallest environment that satisfies Load.
func serveEnv(t *testing.T) {
	t.Helper()
	setAll(t)
	t.Setenv("PATCH_DATABASE_URL", "postgres://patch_ro@postgres/config")
	t.Setenv("PATCH_BLOB_ROOT", "/var/lib/otomo/blobs")
	t.Setenv("PATCH_STAFF_JWKS_URL", "http://php-admin/.well-known/jwks.json")
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
		DatabaseURL:     "postgres://patch_ro@postgres/config",
		DBMaxConns:      4,
		BlobRoot:        "/var/lib/otomo/blobs",
		InternalAddr:    ":8081",
		StaffJWKSURL:    "http://php-admin/.well-known/jwks.json",
		StaffIssuer:     "https://admin-auth.otomo.internal",
		StaffAudience:   "otomo:staff",
		JWKSRefresh:     30 * time.Second,
		JWTClockSkew:    30 * time.Second,
		PollInterval:    60 * time.Second,
		ReadTimeout:     10 * time.Second,
		WriteTimeout:    30 * time.Second,
		IdleTimeout:     120 * time.Second,
		ShutdownTimeout: 15 * time.Second,
		LogLevel:        slog.LevelInfo,
	}
	if cfg != want {
		t.Errorf("Load() = %+v\nwant %+v", cfg, want)
	}
}

// TestLoadRequiresTheReadOnlyDatabaseAndBlobRoot keeps the read-only boundary honest:
// Patch cannot serve without the patch_ro URL and the volume Config writes blobs to,
// and neither has a default that could point at the wrong thing.
func TestLoadRequiresTheReadOnlyDatabaseAndBlobRoot(t *testing.T) {
	setAll(t)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() accepted an environment with no database, blob root or jwks")
	}
	for _, want := range []string{
		"PATCH_DATABASE_URL is required",
		"PATCH_BLOB_ROOT is required",
		"PATCH_STAFF_JWKS_URL is required",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestLoadReportsEveryProblemAtOnce is the property that makes a failed start-up
// useful: a container that reports one problem per restart makes an operator iterate
// through their environment file one line at a time.
func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	serveEnv(t)
	t.Setenv("PATCH_STAFF_JWKS_URL", "not-a-url")
	t.Setenv("PATCH_DB_MAX_CONNS", "lots")
	t.Setenv("PATCH_READ_TIMEOUT", "10 seconds")
	t.Setenv("PATCH_SHUTDOWN_TIMEOUT", "-5s")
	t.Setenv("PATCH_JWT_CLOCK_SKEW", "-1s")
	t.Setenv("PATCH_POLL_INTERVAL", "0s")
	t.Setenv("PATCH_LOG_LEVEL", "verbose")

	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted an environment full of bad values")
	}
	for _, want := range []string{
		"PATCH_DB_MAX_CONNS",
		"PATCH_READ_TIMEOUT",
		"PATCH_SHUTDOWN_TIMEOUT",
		"PATCH_JWT_CLOCK_SKEW",
		"PATCH_POLL_INTERVAL",
		"PATCH_LOG_LEVEL",
		"PATCH_STAFF_JWKS_URL",
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
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			serveEnv(t)
			t.Setenv("PATCH_STAFF_JWKS_URL", raw)

			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "absolute URL") {
				t.Fatalf("Load() error = %v, want an absolute-URL complaint", err)
			}
		})
	}
}

func TestLoadAcceptsEveryValidValue(t *testing.T) {
	serveEnv(t)
	t.Setenv("PATCH_LISTEN_ADDR", "127.0.0.1:0")
	t.Setenv("PATCH_METRICS_ADDR", "127.0.0.1:0")
	t.Setenv("PATCH_DB_MAX_CONNS", "3")
	t.Setenv("PATCH_STAFF_JWKS_REFRESH", "5m")
	t.Setenv("PATCH_JWT_CLOCK_SKEW", "0s")
	t.Setenv("PATCH_POLL_INTERVAL", "15s")
	t.Setenv("PATCH_LOG_LEVEL", "debug")

	// A zero clock skew is the one value that must survive a "positive" check: it is
	// a legitimate choice for a deployment whose containers share a clock source.
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:0" || cfg.DBMaxConns != 3 || cfg.BlobRoot != "/var/lib/otomo/blobs" {
		t.Errorf("Load() = %+v", cfg)
	}
	if cfg.JWKSRefresh != 5*time.Minute || cfg.PollInterval != 15*time.Second {
		t.Errorf("JWKSRefresh = %s, PollInterval = %s; want 5m and 15s", cfg.JWKSRefresh, cfg.PollInterval)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %s, want debug", cfg.LogLevel)
	}
}

// TestLoadTreatsEmptyAsUnset is what stops an empty variable in a compose file from
// blanking out a default and leaving the service with no listener address.
func TestLoadTreatsEmptyAsUnset(t *testing.T) {
	serveEnv(t)
	t.Setenv("PATCH_LISTEN_ADDR", "")
	t.Setenv("PATCH_DB_MAX_CONNS", "")
	t.Setenv("PATCH_STAFF_ISSUER", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want the default :8080", cfg.ListenAddr)
	}
	if cfg.DBMaxConns != 4 {
		t.Errorf("DBMaxConns = %d, want the default 4", cfg.DBMaxConns)
	}
	if cfg.StaffIssuer != "https://admin-auth.otomo.internal" {
		t.Errorf("StaffIssuer = %q, want the default", cfg.StaffIssuer)
	}
}
