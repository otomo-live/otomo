package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// The required values and the non-secret defaults a test asserts against. Every test
// sets the two required variables before calling Load unless it is specifically about
// them being missing.
const (
	testDatabaseURL     = "postgres://allocator:pw@postgres:5432/allocator?sslmode=disable"
	testPublicAddress   = "gameplay.example.com"
	testSessionKeyPath  = "/run/secrets/allocator_session.key"
	testGameServerKey   = "/run/secrets/allocator_gameserver.key"
	testCallbackKeyPath = "/run/secrets/session_allocator.key"
	testProxyKeyPath    = "/run/secrets/allocator_proxy.key"
)

// allVars is every variable Load reads, so a test can pin the environment instead of
// inheriting whatever the developer has exported. t.Setenv with an empty value is the
// same as unsetting it: every helper treats "" as "use the default".
var allVars = []string{
	"ALLOCATOR_DATABASE_URL",
	"ALLOCATOR_LISTEN_ADDR",
	"ALLOCATOR_METRICS_ADDR",
	"ALLOCATOR_LOG_LEVEL",
	"ALLOCATOR_DB_MAX_CONNS",
	"ALLOCATOR_SIGNING_KEY_PATH",
	"ALLOCATOR_SESSION_KEY_PATH",
	"ALLOCATOR_GAMESERVER_KEY_PATH",
	"ALLOCATOR_CALLBACK_KEY_PATH",
	"ALLOCATOR_PROXY_KEY_PATH",
	"ALLOCATOR_SESSION_URL",
	"ALLOCATOR_PUBLIC_ADDRESS",
	"ALLOCATOR_PUBLIC_PORT",
	"ALLOCATOR_ISSUER",
	"ALLOCATOR_AUDIENCE",
	"ALLOCATOR_TICKET_TTL",
	"ALLOCATOR_RESERVATION_TTL",
	"ALLOCATOR_HEARTBEAT_TIMEOUT",
	"ALLOCATOR_REAP_INTERVAL",
	"ALLOCATOR_READ_TIMEOUT",
	"ALLOCATOR_WRITE_TIMEOUT",
	"ALLOCATOR_IDLE_TIMEOUT",
	"ALLOCATOR_SHUTDOWN_TIMEOUT",
}

// envs turns a flat key/value list into the override map load expects. It keeps the
// large override test readable without a map literal whose alignment shifts every time
// a key is renamed.
func envs(pairs ...string) map[string]string {
	if len(pairs)%2 != 0 {
		panic("envs needs an even number of arguments")
	}
	m := make(map[string]string, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1]
	}
	return m
}

// load runs Load against a clean environment plus the required variables and whatever
// overrides the caller wants to exercise.
func load(t *testing.T, overrides map[string]string) (Config, error) {
	t.Helper()

	for _, key := range allVars {
		t.Setenv(key, "")
	}
	t.Setenv("ALLOCATOR_DATABASE_URL", testDatabaseURL)
	t.Setenv("ALLOCATOR_PUBLIC_ADDRESS", testPublicAddress)
	for key, value := range overrides {
		t.Setenv(key, value)
	}
	return Load()
}

// TestLoadDefaults pins every default in one place. It is the contract an operator
// reads when a container is started with nothing but the two required variables.
func TestLoadDefaults(t *testing.T) {
	cfg, err := load(t, nil)
	if err != nil {
		t.Fatalf("Load() returned %v with only the required variables set", err)
	}

	want := Config{
		DatabaseURL:       testDatabaseURL,
		ListenAddr:        ":8080",
		MetricsAddr:       ":9090",
		LogLevel:          slog.LevelInfo,
		DBMaxConns:        8,
		SigningKeyPath:    "/run/secrets/allocator/signing_key.pem",
		SessionKeyPath:    testSessionKeyPath,
		GameServerKeyPath: testGameServerKey,
		CallbackKeyPath:   testCallbackKeyPath,
		ProxyKeyPath:      testProxyKeyPath,
		SessionURL:        "http://session:8081",
		PublicAddress:     testPublicAddress,
		PublicPort:        27000,
		Issuer:            "https://allocator.otomo.internal",
		Audience:          "otomo:gameserver",
		TicketTTL:         60 * time.Second,
		ReservationTTL:    60 * time.Second,
		HeartbeatTimeout:  15 * time.Second,
		ReapInterval:      5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		ShutdownTimeout:   15 * time.Second,
	}
	if cfg != want {
		t.Errorf("Load() = %+v, want %+v", cfg, want)
	}
}

// TestLoadReportsEveryMissingRequiredVar checks the promise Load makes: one start-up
// names every missing variable, not just the first one it happens to read.
func TestLoadReportsEveryMissingRequiredVar(t *testing.T) {
	for _, key := range allVars {
		t.Setenv(key, "")
	}

	_, err := Load()
	if err == nil {
		t.Fatal("Load() accepted an empty environment")
	}
	for _, want := range []string{"ALLOCATOR_DATABASE_URL", "ALLOCATOR_PUBLIC_ADDRESS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
}

// TestLoadRejectsBadValues feeds one malformed or out-of-range value at a time and
// checks it is named in the returned error. Grouping them in one table keeps the
// message wording under test without one test function per variable.
func TestLoadRejectsBadValues(t *testing.T) {
	cases := []struct {
		name      string
		key       string
		value     string
		wantInErr string
	}{
		{"bad log level", "ALLOCATOR_LOG_LEVEL", "loud", "ALLOCATOR_LOG_LEVEL"},
		{"bad max conns", "ALLOCATOR_DB_MAX_CONNS", "lots", "ALLOCATOR_DB_MAX_CONNS"},
		{"zero max conns", "ALLOCATOR_DB_MAX_CONNS", "0", "ALLOCATOR_DB_MAX_CONNS"},
		{"too many max conns", "ALLOCATOR_DB_MAX_CONNS", "101", "ALLOCATOR_DB_MAX_CONNS"},
		{"bad public port", "ALLOCATOR_PUBLIC_PORT", "http", "ALLOCATOR_PUBLIC_PORT"},
		{"zero public port", "ALLOCATOR_PUBLIC_PORT", "0", "ALLOCATOR_PUBLIC_PORT"},
		{"too large public port", "ALLOCATOR_PUBLIC_PORT", "70000", "ALLOCATOR_PUBLIC_PORT"},
		{"bad session URL", "ALLOCATOR_SESSION_URL", "session:8081", "ALLOCATOR_SESSION_URL"},
		{"non-http session URL", "ALLOCATOR_SESSION_URL", "grpc://session:8081", "ALLOCATOR_SESSION_URL"},
		{"bad shutdown timeout", "ALLOCATOR_SHUTDOWN_TIMEOUT", "soon", "ALLOCATOR_SHUTDOWN_TIMEOUT"},
		{"zero shutdown timeout", "ALLOCATOR_SHUTDOWN_TIMEOUT", "0s", "ALLOCATOR_SHUTDOWN_TIMEOUT"},
		{"negative read timeout", "ALLOCATOR_READ_TIMEOUT", "-1s", "ALLOCATOR_READ_TIMEOUT"},
		{"ticket TTL too short", "ALLOCATOR_TICKET_TTL", "5s", "ALLOCATOR_TICKET_TTL"},
		{"ticket TTL too long", "ALLOCATOR_TICKET_TTL", "1h", "ALLOCATOR_TICKET_TTL"},
		{"reservation TTL too long", "ALLOCATOR_RESERVATION_TTL", "11m", "ALLOCATOR_RESERVATION_TTL"},
		{"bad heartbeat timeout", "ALLOCATOR_HEARTBEAT_TIMEOUT", "0s", "ALLOCATOR_HEARTBEAT_TIMEOUT"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(t, map[string]string{tc.key: tc.value})
			if err == nil {
				t.Fatalf("Load() accepted %s=%q", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.wantInErr) {
				t.Errorf("error does not name %s: %v", tc.wantInErr, err)
			}
		})
	}
}

// TestLoadRejectsReapIntervalNotShorterThanHeartbeat covers the one rule that spans
// two variables, in both the equal and the longer case.
func TestLoadRejectsReapIntervalNotShorterThanHeartbeat(t *testing.T) {
	for _, reap := range []string{"10s", "30s"} {
		t.Run(reap, func(t *testing.T) {
			_, err := load(t, map[string]string{
				"ALLOCATOR_HEARTBEAT_TIMEOUT": "10s",
				"ALLOCATOR_REAP_INTERVAL":     reap,
			})
			if err == nil {
				t.Fatalf("Load() accepted REAP_INTERVAL=%s with HEARTBEAT_TIMEOUT=10s", reap)
			}
			if !strings.Contains(err.Error(), "ALLOCATOR_REAP_INTERVAL") {
				t.Errorf("error does not name ALLOCATOR_REAP_INTERVAL: %v", err)
			}
		})
	}
}

// TestLoadAcceptsOverrides is the happy path for a tuned deployment: every value
// differs from its default and Load must still succeed and report what was set.
func TestLoadAcceptsOverrides(t *testing.T) {
	cfg, err := load(t, envs(
		"ALLOCATOR_LISTEN_ADDR", "127.0.0.1:9000",
		"ALLOCATOR_METRICS_ADDR", "127.0.0.1:9001",
		"ALLOCATOR_LOG_LEVEL", "debug",
		"ALLOCATOR_DB_MAX_CONNS", "100",
		"ALLOCATOR_SIGNING_KEY_PATH", "/tmp/signing.pem",
		"ALLOCATOR_SESSION_KEY_PATH", "/tmp/session.key",
		"ALLOCATOR_GAMESERVER_KEY_PATH", "/tmp/gameserver.key",
		"ALLOCATOR_CALLBACK_KEY_PATH", "/tmp/callback.key",
		"ALLOCATOR_PROXY_KEY_PATH", "/tmp/proxy.key",
		"ALLOCATOR_SESSION_URL", "https://session.internal:8443",
		"ALLOCATOR_PUBLIC_ADDRESS", "10.0.0.5",
		"ALLOCATOR_PUBLIC_PORT", "27015",
		"ALLOCATOR_ISSUER", "https://allocator.example.com",
		"ALLOCATOR_AUDIENCE", "otomo:gameserver:test",
		"ALLOCATOR_TICKET_TTL", "10s",
		"ALLOCATOR_RESERVATION_TTL", "10m",
		"ALLOCATOR_HEARTBEAT_TIMEOUT", "30s",
		"ALLOCATOR_REAP_INTERVAL", "29s",
		"ALLOCATOR_READ_TIMEOUT", "1s",
		"ALLOCATOR_WRITE_TIMEOUT", "2s",
		"ALLOCATOR_IDLE_TIMEOUT", "3s",
		"ALLOCATOR_SHUTDOWN_TIMEOUT", "4s",
	))
	if err != nil {
		t.Fatalf("Load() returned %v", err)
	}

	if cfg.PublicPort != 27015 {
		t.Errorf("PublicPort = %d, want 27015", cfg.PublicPort)
	}
	if cfg.DBMaxConns != 100 {
		t.Errorf("DBMaxConns = %d, want 100", cfg.DBMaxConns)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want debug", cfg.LogLevel)
	}
	if cfg.ProxyKeyPath != "/tmp/proxy.key" {
		t.Errorf("ProxyKeyPath = %q, want /tmp/proxy.key", cfg.ProxyKeyPath)
	}
	if cfg.TicketTTL != 10*time.Second {
		t.Errorf("TicketTTL = %s, want 10s", cfg.TicketTTL)
	}
	if cfg.ReservationTTL != 10*time.Minute {
		t.Errorf("ReservationTTL = %s, want 10m", cfg.ReservationTTL)
	}
}

// TestLoadDatabaseNeedsOnlyTheDatabaseURL is the point of LoadDatabase: migrate must
// run on a host that has no ALLOCATOR_PUBLIC_ADDRESS and no key files mounted, so Load
// would reject an environment migrate has to accept.
func TestLoadDatabaseNeedsOnlyTheDatabaseURL(t *testing.T) {
	for _, key := range allVars {
		t.Setenv(key, "")
	}
	t.Setenv("ALLOCATOR_DATABASE_URL", testDatabaseURL)

	cfg, err := LoadDatabase()
	if err != nil {
		t.Fatalf("LoadDatabase() returned %v with only ALLOCATOR_DATABASE_URL set", err)
	}
	if cfg.DatabaseURL != testDatabaseURL {
		t.Errorf("DatabaseURL = %q, want %q", cfg.DatabaseURL, testDatabaseURL)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", cfg.LogLevel)
	}

	// The default would pass the assertion above by coincidence, so pin that the
	// configured level is actually read.
	t.Setenv("ALLOCATOR_LOG_LEVEL", "debug")
	cfg, err = LoadDatabase()
	if err != nil {
		t.Fatalf("LoadDatabase() returned %v with ALLOCATOR_LOG_LEVEL set", err)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want debug", cfg.LogLevel)
	}
}

// TestLoadDatabaseRequiresTheDatabaseURL is the one thing migrate cannot do without.
func TestLoadDatabaseRequiresTheDatabaseURL(t *testing.T) {
	for _, key := range allVars {
		t.Setenv(key, "")
	}

	_, err := LoadDatabase()
	if err == nil {
		t.Fatal("LoadDatabase() accepted an environment with no ALLOCATOR_DATABASE_URL")
	}
	if !strings.Contains(err.Error(), "ALLOCATOR_DATABASE_URL") {
		t.Errorf("error does not name ALLOCATOR_DATABASE_URL: %v", err)
	}
}
