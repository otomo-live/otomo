package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// allVars is every variable Load reads, so a test can pin the environment instead of
// inheriting whatever the developer has exported. t.Setenv with an empty value is the
// same as unsetting it: every helper treats "" as "use the default".
var allVars = []string{
	"PROXY_LISTEN_ADDR",
	"PROXY_METRICS_ADDR",
	"PROXY_ALLOCATOR_URL",
	"PROXY_ALLOCATOR_KEY_PATH",
	"PROXY_TICKET_ISSUER",
	"PROXY_TICKET_AUDIENCE",
	"PROXY_IDLE_TIMEOUT",
	"PROXY_DIRECTORY_REFRESH",
	"PROXY_MAX_SESSIONS",
	"PROXY_SHUTDOWN_TIMEOUT",
	"PROXY_LOG_LEVEL",
}

// clear sets every variable Load reads to empty and returns nothing, so each test
// starts from a known environment.
func clear(t *testing.T) {
	t.Helper()
	for _, key := range allVars {
		t.Setenv(key, "")
	}
}

// TestLoadDefaults pins every default in one place. It is the contract an operator
// reads when a container is started with no proxy variables at all.
func TestLoadDefaults(t *testing.T) {
	clear(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned %v with an empty environment", err)
	}

	want := Config{
		ListenAddr:       ":27000",
		MetricsAddr:      ":9090",
		AllocatorURL:     "http://allocator:8080",
		AllocatorKeyPath: "/run/secrets/allocator_proxy.key",
		Issuer:           "https://allocator.otomo.internal",
		Audience:         "otomo:gameserver",
		IdleTimeout:      30 * time.Second,
		DirectoryRefresh: 5 * time.Second,
		MaxSessions:      2000,
		ShutdownTimeout:  15 * time.Second,
		LogLevel:         slog.LevelInfo,
	}
	if cfg != want {
		t.Errorf("Load() = %+v, want %+v", cfg, want)
	}
}

// TestLoadRejectsBadValues feeds one malformed or out-of-range value at a time and
// checks it is named in the returned error.
func TestLoadRejectsBadValues(t *testing.T) {
	cases := []struct {
		name      string
		key       string
		value     string
		wantInErr string
	}{
		{"bad allocator URL", "PROXY_ALLOCATOR_URL", "allocator:8080", "PROXY_ALLOCATOR_URL"},
		{"non-http allocator URL", "PROXY_ALLOCATOR_URL", "grpc://allocator:8080", "PROXY_ALLOCATOR_URL"},
		{"bad idle timeout", "PROXY_IDLE_TIMEOUT", "soon", "PROXY_IDLE_TIMEOUT"},
		{"zero idle timeout", "PROXY_IDLE_TIMEOUT", "0s", "PROXY_IDLE_TIMEOUT"},
		{"negative idle timeout", "PROXY_IDLE_TIMEOUT", "-1s", "PROXY_IDLE_TIMEOUT"},
		{"bad directory refresh", "PROXY_DIRECTORY_REFRESH", "often", "PROXY_DIRECTORY_REFRESH"},
		{"zero directory refresh", "PROXY_DIRECTORY_REFRESH", "0s", "PROXY_DIRECTORY_REFRESH"},
		{"bad max sessions", "PROXY_MAX_SESSIONS", "lots", "PROXY_MAX_SESSIONS"},
		{"zero max sessions", "PROXY_MAX_SESSIONS", "0", "PROXY_MAX_SESSIONS"},
		{"too many max sessions", "PROXY_MAX_SESSIONS", "1000001", "PROXY_MAX_SESSIONS"},
		{"bad shutdown timeout", "PROXY_SHUTDOWN_TIMEOUT", "soon", "PROXY_SHUTDOWN_TIMEOUT"},
		{"bad log level", "PROXY_LOG_LEVEL", "loud", "PROXY_LOG_LEVEL"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clear(t)
			t.Setenv(tc.key, tc.value)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() accepted %s=%q", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.wantInErr) {
				t.Errorf("error does not name %s: %v", tc.wantInErr, err)
			}
		})
	}
}

// TestLoadReportsEveryProblem checks the promise Load makes: one start-up names every
// bad variable, not just the first one it happens to read.
func TestLoadReportsEveryProblem(t *testing.T) {
	clear(t)
	t.Setenv("PROXY_ALLOCATOR_URL", "not a url")
	t.Setenv("PROXY_MAX_SESSIONS", "0")
	t.Setenv("PROXY_IDLE_TIMEOUT", "0s")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() accepted an invalid environment")
	}
	for _, want := range []string{"PROXY_ALLOCATOR_URL", "PROXY_MAX_SESSIONS", "PROXY_IDLE_TIMEOUT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
}

// TestLoadAcceptsOverrides is the happy path for a tuned deployment: every value
// differs from its default and Load must still succeed and report what was set.
func TestLoadAcceptsOverrides(t *testing.T) {
	clear(t)
	t.Setenv("PROXY_LISTEN_ADDR", "127.0.0.1:9000")
	t.Setenv("PROXY_METRICS_ADDR", "127.0.0.1:9001")
	t.Setenv("PROXY_ALLOCATOR_URL", "https://allocator.internal:8443/")
	t.Setenv("PROXY_ALLOCATOR_KEY_PATH", "/tmp/proxy.key")
	t.Setenv("PROXY_TICKET_ISSUER", "https://allocator.example.com")
	t.Setenv("PROXY_TICKET_AUDIENCE", "otomo:gameserver:test")
	t.Setenv("PROXY_IDLE_TIMEOUT", "1m")
	t.Setenv("PROXY_DIRECTORY_REFRESH", "2s")
	t.Setenv("PROXY_MAX_SESSIONS", "7")
	t.Setenv("PROXY_SHUTDOWN_TIMEOUT", "3s")
	t.Setenv("PROXY_LOG_LEVEL", "debug")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:9000" {
		t.Errorf("ListenAddr = %q", cfg.ListenAddr)
	}
	if cfg.AllocatorURL != "https://allocator.internal:8443/" {
		t.Errorf("AllocatorURL = %q", cfg.AllocatorURL)
	}
	if cfg.IdleTimeout != time.Minute {
		t.Errorf("IdleTimeout = %s, want 1m", cfg.IdleTimeout)
	}
	if cfg.DirectoryRefresh != 2*time.Second {
		t.Errorf("DirectoryRefresh = %s, want 2s", cfg.DirectoryRefresh)
	}
	if cfg.MaxSessions != 7 {
		t.Errorf("MaxSessions = %d, want 7", cfg.MaxSessions)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want debug", cfg.LogLevel)
	}
}
