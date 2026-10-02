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
		"CONFIG_LISTEN_ADDR", "CONFIG_METRICS_ADDR", "CONFIG_DATABASE_URL",
		"CONFIG_DB_MAX_CONNS", "CONFIG_BLOB_ROOT", "CONFIG_STAFF_JWKS_URL",
		"CONFIG_MAX_PACK_BYTES",
		"CONFIG_STAFF_ISSUER", "CONFIG_STAFF_AUDIENCE", "CONFIG_STAFF_JWKS_REFRESH",
		"CONFIG_JWT_CLOCK_SKEW", "CONFIG_READ_TIMEOUT", "CONFIG_WRITE_TIMEOUT",
		"CONFIG_IDLE_TIMEOUT", "CONFIG_SHUTDOWN_TIMEOUT", "CONFIG_LOG_LEVEL",
	} {
		t.Setenv(key, "")
	}
}

// serveEnv is the smallest environment that satisfies ModeServe.
func serveEnv(t *testing.T) {
	t.Helper()
	setAll(t)
	t.Setenv("CONFIG_DATABASE_URL", "postgres://config@postgres/config")
	t.Setenv("CONFIG_STAFF_JWKS_URL", "http://php-admin/.well-known/jwks.json")
	t.Setenv("CONFIG_STAFF_ISSUER", "https://php-admin.otomo.internal")
	t.Setenv("CONFIG_STAFF_AUDIENCE", "otomo:staff")
}

func TestLoadDefaults(t *testing.T) {
	setAll(t)
	t.Setenv("CONFIG_DATABASE_URL", "postgres://config@postgres/config")

	cfg, err := Load(ModeMigrate)
	if err != nil {
		t.Fatalf("Load(ModeMigrate) = %v", err)
	}

	want := Config{
		ListenAddr:      ":8080",
		MetricsAddr:     ":9090",
		DatabaseURL:     "postgres://config@postgres/config",
		DBMaxConns:      8,
		BlobRoot:        "/var/lib/otomo/blobs",
		MaxPackBytes:    536870912,
		JWKSRefresh:     30 * time.Second,
		JWTClockSkew:    30 * time.Second,
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

// TestLoadServingRequiresTheStaffDomain keeps CFG-A1's boundary honest: the three
// staff variables have no defaults, because a wrong issuer or audience value would be
// accepted silently and every request would then 401 with no clue why.
func TestLoadServingRequiresTheStaffDomain(t *testing.T) {
	setAll(t)
	t.Setenv("CONFIG_DATABASE_URL", "postgres://config@postgres/config")

	_, err := Load(ModeServe)
	if err == nil {
		t.Fatal("Load(ModeServe) accepted an environment with no staff identity domain")
	}
	for _, want := range []string{
		"CONFIG_STAFF_JWKS_URL is required",
		"CONFIG_STAFF_ISSUER is required",
		"CONFIG_STAFF_AUDIENCE is required",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestLoadMigrateNeedsOnlyTheDatabase is the reason Mode exists. The migration job
// runs before the service is deployed and has no blob volume and no reason to know
// the staff issuer; requiring them would make the Jenkins job fail for a reason that
// has nothing to do with migrating.
func TestLoadMigrateNeedsOnlyTheDatabase(t *testing.T) {
	setAll(t)

	if _, err := Load(ModeMigrate); err == nil || !strings.Contains(err.Error(), "CONFIG_DATABASE_URL is required") {
		t.Fatalf("Load(ModeMigrate) error = %v, want CONFIG_DATABASE_URL is required", err)
	}

	t.Setenv("CONFIG_DATABASE_URL", "postgres://config@postgres/config")
	cfg, err := Load(ModeMigrate)
	if err != nil {
		t.Fatalf("Load(ModeMigrate) = %v", err)
	}
	if cfg.StaffJWKSURL != "" || cfg.StaffIssuer != "" || cfg.StaffAudience != "" {
		t.Errorf("Load(ModeMigrate) invented a staff domain: %+v", cfg)
	}
}

// TestLoadReportsEveryProblemAtOnce is the property that makes a failed start-up
// useful: a container that reports one problem per restart makes an operator iterate
// through their environment file one line at a time.
func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	setAll(t)
	t.Setenv("CONFIG_DATABASE_URL", "postgres://config@postgres/config")
	t.Setenv("CONFIG_STAFF_JWKS_URL", "not-a-url")
	t.Setenv("CONFIG_STAFF_ISSUER", "https://php-admin.otomo.internal")
	t.Setenv("CONFIG_STAFF_AUDIENCE", "otomo:staff")
	t.Setenv("CONFIG_DB_MAX_CONNS", "lots")
	t.Setenv("CONFIG_MAX_PACK_BYTES", "0")
	t.Setenv("CONFIG_READ_TIMEOUT", "10 seconds")
	t.Setenv("CONFIG_SHUTDOWN_TIMEOUT", "-5s")
	t.Setenv("CONFIG_JWT_CLOCK_SKEW", "-1s")
	t.Setenv("CONFIG_LOG_LEVEL", "verbose")

	_, err := Load(ModeServe)
	if err == nil {
		t.Fatal("Load accepted an environment full of bad values")
	}
	for _, want := range []string{
		"CONFIG_DB_MAX_CONNS",
		"CONFIG_MAX_PACK_BYTES",
		"CONFIG_READ_TIMEOUT",
		"CONFIG_SHUTDOWN_TIMEOUT",
		"CONFIG_JWT_CLOCK_SKEW",
		"CONFIG_LOG_LEVEL",
		"CONFIG_STAFF_JWKS_URL",
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
			t.Setenv("CONFIG_STAFF_JWKS_URL", url)

			if _, err := Load(ModeServe); err == nil || !strings.Contains(err.Error(), "absolute URL") {
				t.Fatalf("Load() error = %v, want an absolute-URL complaint", err)
			}
		})
	}
}

func TestLoadAcceptsEveryValidValue(t *testing.T) {
	serveEnv(t)
	t.Setenv("CONFIG_LISTEN_ADDR", "127.0.0.1:0")
	t.Setenv("CONFIG_METRICS_ADDR", "127.0.0.1:0")
	t.Setenv("CONFIG_DB_MAX_CONNS", "3")
	t.Setenv("CONFIG_BLOB_ROOT", "/srv/blobs")
	t.Setenv("CONFIG_STAFF_JWKS_REFRESH", "5m")
	t.Setenv("CONFIG_JWT_CLOCK_SKEW", "0s")
	t.Setenv("CONFIG_LOG_LEVEL", "debug")

	// A zero clock skew is the one value that must survive a "positive" check: it is
	// a legitimate choice for a deployment whose containers share a clock source.
	cfg, err := Load(ModeServe)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:0" || cfg.DBMaxConns != 3 || cfg.BlobRoot != "/srv/blobs" {
		t.Errorf("Load() = %+v", cfg)
	}
	if cfg.JWKSRefresh != 5*time.Minute {
		t.Errorf("JWKSRefresh = %s, want 5m", cfg.JWKSRefresh)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %s, want debug", cfg.LogLevel)
	}
}

// TestLoadTreatsEmptyAsUnset is what stops an empty variable in a compose file from
// blanking out a default and leaving the service with no listener address.
func TestLoadTreatsEmptyAsUnset(t *testing.T) {
	setAll(t)
	t.Setenv("CONFIG_DATABASE_URL", "postgres://config@postgres/config")
	t.Setenv("CONFIG_LISTEN_ADDR", "")
	t.Setenv("CONFIG_DB_MAX_CONNS", "")

	cfg, err := Load(ModeMigrate)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want the default :8080", cfg.ListenAddr)
	}
	if cfg.DBMaxConns != 8 {
		t.Errorf("DBMaxConns = %d, want the default 8", cfg.DBMaxConns)
	}
}
