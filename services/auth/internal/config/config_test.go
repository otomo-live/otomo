package config_test

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/auth/internal/config"
)

var envVars = []string{
	"AUTH_LISTEN_ADDR",
	"AUTH_METRICS_ADDR",
	"AUTH_DATABASE_URL",
	"AUTH_DB_MAX_CONNS",
	"AUTH_SIGNING_KEY_PATH",
	"AUTH_ISSUER",
	"AUTH_AUDIENCE",
	"AUTH_ACCESS_TOKEN_TTL",
	"AUTH_REFRESH_TOKEN_TTL",
	"AUTH_PUBLIC_SESSION_URL",
	"AUTH_READ_TIMEOUT",
	"AUTH_WRITE_TIMEOUT",
	"AUTH_IDLE_TIMEOUT",
	"AUTH_SHUTDOWN_TIMEOUT",
	"AUTH_LOG_LEVEL",
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range envVars {
		t.Setenv(key, "")
	}
}

func TestLoadEmptyEnvNamesEveryRequiredVar(t *testing.T) {
	clearEnv(t)

	_, err := config.Load(config.ModeServe)
	if err == nil {
		t.Fatal("Load with an empty environment returned no error")
	}
	for _, want := range []string{"AUTH_DATABASE_URL", "AUTH_SIGNING_KEY_PATH"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_ACCESS_TOKEN_TTL", "soon")
	t.Setenv("AUTH_DB_MAX_CONNS", "lots")
	t.Setenv("AUTH_LOG_LEVEL", "verbose")

	_, err := config.Load(config.ModeServe)
	if err == nil {
		t.Fatal("Load with invalid values returned no error")
	}
	for _, want := range []string{"AUTH_ACCESS_TOKEN_TTL", "AUTH_DB_MAX_CONNS", "AUTH_LOG_LEVEL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

// TestServeNoLongerNeedsAPublicSessionURL: the services hand-off was dropped (D2), so
// serve starts without AUTH_PUBLIC_SESSION_URL, and a value left over in an old
// environment is ignored rather than validated.
func TestServeNoLongerNeedsAPublicSessionURL(t *testing.T) {
	for _, leftover := range []string{"", "not a url"} {
		clearEnv(t)
		t.Setenv("AUTH_DATABASE_URL", "postgres://auth_rw:pw@postgres:5432/auth")
		t.Setenv("AUTH_SIGNING_KEY_PATH", "/run/secrets/auth_signing_key.pem")
		t.Setenv("AUTH_PUBLIC_SESSION_URL", leftover)
		if _, err := config.Load(config.ModeServe); err != nil {
			t.Errorf("Load(serve) with AUTH_PUBLIC_SESSION_URL=%q: %v", leftover, err)
		}
	}
}

func TestLoadServeDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_DATABASE_URL", "postgres://auth_rw:pw@postgres:5432/auth")
	t.Setenv("AUTH_SIGNING_KEY_PATH", "/run/secrets/auth_signing_key.pem")

	cfg, err := config.Load(config.ModeServe)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"ListenAddr", cfg.ListenAddr, ":8080"},
		{"MetricsAddr", cfg.MetricsAddr, ":9090"},
		{"DBMaxConns", cfg.DBMaxConns, int32(8)},
		{"Issuer", cfg.Issuer, "https://auth.otomo.internal"},
		{"Audience", cfg.Audience, "otomo:player"},
		{"AccessTokenTTL", cfg.AccessTokenTTL, 15 * time.Minute},
		{"RefreshTokenTTL", cfg.RefreshTokenTTL, 720 * time.Hour},
		{"ReadTimeout", cfg.ReadTimeout, 10 * time.Second},
		{"WriteTimeout", cfg.WriteTimeout, 30 * time.Second},
		{"IdleTimeout", cfg.IdleTimeout, 120 * time.Second},
		{"ShutdownTimeout", cfg.ShutdownTimeout, 15 * time.Second},
		{"LogLevel", cfg.LogLevel, slog.LevelInfo},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestLoadGenkeyDoesNotRequireSigningKeyPath(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_DATABASE_URL", "postgres://auth_rw:pw@postgres:5432/auth")

	cfg, err := config.Load(config.ModeGenkey)
	if err != nil {
		t.Fatalf("Load(ModeGenkey): %v", err)
	}
	if cfg.SigningKeyPath != "" {
		t.Errorf("SigningKeyPath = %q, want empty", cfg.SigningKeyPath)
	}
}
