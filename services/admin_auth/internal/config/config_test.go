package config_test

import (
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/admin_auth/internal/config"
)

var envVars = []string{
	"ADMIN_AUTH_LISTEN_ADDR",
	"ADMIN_AUTH_METRICS_ADDR",
	"ADMIN_AUTH_DATABASE_URL",
	"ADMIN_AUTH_DB_MAX_CONNS",
	"ADMIN_AUTH_SIGNING_KEY_PATH",
	"ADMIN_AUTH_TOTP_KEY_PATH",
	"ADMIN_AUTH_ROOT_PASSWORD_FILE",
	"ADMIN_AUTH_ISSUER",
	"ADMIN_AUTH_AUDIENCE",
	"ADMIN_AUTH_ACCESS_TOKEN_TTL",
	"ADMIN_AUTH_REFRESH_TOKEN_TTL",
	"ADMIN_AUTH_REFRESH_REUSE_GRACE",
	"ADMIN_AUTH_INVITE_TTL",
	"ADMIN_AUTH_PUBLIC_URL",
	"ADMIN_AUTH_LOGIN_MAX_FAILURES",
	"ADMIN_AUTH_LOGIN_LOCKOUT",
	"ADMIN_AUTH_READ_TIMEOUT",
	"ADMIN_AUTH_WRITE_TIMEOUT",
	"ADMIN_AUTH_IDLE_TIMEOUT",
	"ADMIN_AUTH_SHUTDOWN_TIMEOUT",
	"ADMIN_AUTH_LOG_LEVEL",
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range envVars {
		t.Setenv(key, "")
	}
}

// writeTOTPKey writes a valid 32-byte base64 TOTP key to a temp file and returns its
// path, so a serve-mode Load has the key it now requires.
func writeTOTPKey(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "totp.key")
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(make([]byte, 32))+"\n"), 0o600); err != nil {
		t.Fatalf("write TOTP key: %v", err)
	}
	return path
}

func TestLoadEmptyEnvNamesEveryRequiredVar(t *testing.T) {
	clearEnv(t)

	_, err := config.Load(config.ModeServe)
	if err == nil {
		t.Fatal("Load with an empty environment returned no error")
	}
	if !strings.Contains(err.Error(), "ADMIN_AUTH_DATABASE_URL") {
		t.Errorf("error %q does not name ADMIN_AUTH_DATABASE_URL", err)
	}
	if !strings.Contains(err.Error(), "ADMIN_AUTH_SIGNING_KEY_PATH") {
		t.Errorf("error %q does not name ADMIN_AUTH_SIGNING_KEY_PATH", err)
	}
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADMIN_AUTH_DB_MAX_CONNS", "lots")
	t.Setenv("ADMIN_AUTH_READ_TIMEOUT", "soon")
	t.Setenv("ADMIN_AUTH_LOG_LEVEL", "verbose")

	_, err := config.Load(config.ModeServe)
	if err == nil {
		t.Fatal("Load with invalid values returned no error")
	}
	for _, want := range []string{"ADMIN_AUTH_DB_MAX_CONNS", "ADMIN_AUTH_READ_TIMEOUT", "ADMIN_AUTH_LOG_LEVEL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

func TestLoadRejectsNonPositiveDuration(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADMIN_AUTH_DATABASE_URL", "postgres://admin_auth_rw:pw@postgres:5432/admin_auth")
	t.Setenv("ADMIN_AUTH_SHUTDOWN_TIMEOUT", "0s")

	_, err := config.Load(config.ModeServe)
	if err == nil || !strings.Contains(err.Error(), "ADMIN_AUTH_SHUTDOWN_TIMEOUT") {
		t.Fatalf("error = %v, want one naming ADMIN_AUTH_SHUTDOWN_TIMEOUT", err)
	}
}

func TestLoadServeDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADMIN_AUTH_DATABASE_URL", "postgres://admin_auth_rw:pw@postgres:5432/admin_auth")
	t.Setenv("ADMIN_AUTH_SIGNING_KEY_PATH", "/run/secrets/admin_auth_signing_key.pem")
	t.Setenv("ADMIN_AUTH_TOTP_KEY_PATH", writeTOTPKey(t))

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
		{"SigningKeyPath", cfg.SigningKeyPath, "/run/secrets/admin_auth_signing_key.pem"},
		{"Issuer", cfg.Issuer, "https://admin-auth.otomo.internal"},
		{"Audience", cfg.Audience, "otomo:staff"},
		{"AccessTokenTTL", cfg.AccessTokenTTL, 15 * time.Minute},
		{"RefreshTokenTTL", cfg.RefreshTokenTTL, 168 * time.Hour},
		{"RefreshReuseGrace", cfg.RefreshReuseGrace, 30 * time.Second},
		{"InviteTTL", cfg.InviteTTL, 72 * time.Hour},
		{"PublicURL", cfg.PublicURL, "http://localhost:8090"},
		{"LoginMaxFailures", cfg.LoginMaxFailures, 5},
		{"LoginLockout", cfg.LoginLockout, 15 * time.Minute},
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

func TestLoadRejectsTooLongRefreshTokenTTL(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADMIN_AUTH_DATABASE_URL", "postgres://admin_auth_rw:pw@postgres:5432/admin_auth")
	t.Setenv("ADMIN_AUTH_SIGNING_KEY_PATH", "/run/secrets/admin_auth_signing_key.pem")
	t.Setenv("ADMIN_AUTH_REFRESH_TOKEN_TTL", "800h")

	_, err := config.Load(config.ModeServe)
	if err == nil || !strings.Contains(err.Error(), "ADMIN_AUTH_REFRESH_TOKEN_TTL") {
		t.Fatalf("error = %v, want one naming ADMIN_AUTH_REFRESH_TOKEN_TTL", err)
	}
}

func TestLoadRefreshReuseGrace(t *testing.T) {
	base := func(t *testing.T) {
		t.Helper()
		clearEnv(t)
		t.Setenv("ADMIN_AUTH_DATABASE_URL", "postgres://admin_auth_rw:pw@postgres:5432/admin_auth")
		t.Setenv("ADMIN_AUTH_SIGNING_KEY_PATH", "/run/secrets/admin_auth_signing_key.pem")
		t.Setenv("ADMIN_AUTH_TOTP_KEY_PATH", writeTOTPKey(t))
	}

	t.Run("zero disables the grace window", func(t *testing.T) {
		base(t)
		t.Setenv("ADMIN_AUTH_REFRESH_REUSE_GRACE", "0s")

		cfg, err := config.Load(config.ModeServe)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.RefreshReuseGrace != 0 {
			t.Errorf("RefreshReuseGrace = %s, want 0", cfg.RefreshReuseGrace)
		}
	})

	t.Run("value from env", func(t *testing.T) {
		base(t)
		t.Setenv("ADMIN_AUTH_REFRESH_REUSE_GRACE", "45s")

		cfg, err := config.Load(config.ModeServe)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.RefreshReuseGrace != 45*time.Second {
			t.Errorf("RefreshReuseGrace = %s, want 45s", cfg.RefreshReuseGrace)
		}
	})

	for _, c := range []struct {
		name string
		raw  string
	}{
		{"negative", "-1s"},
		{"over the cap", "6m"},
		{"not a duration", "soon"},
	} {
		t.Run(c.name, func(t *testing.T) {
			base(t)
			t.Setenv("ADMIN_AUTH_REFRESH_REUSE_GRACE", c.raw)

			_, err := config.Load(config.ModeServe)
			if err == nil || !strings.Contains(err.Error(), "ADMIN_AUTH_REFRESH_REUSE_GRACE") {
				t.Fatalf("error = %v, want one naming ADMIN_AUTH_REFRESH_REUSE_GRACE", err)
			}
		})
	}
}

func TestLoadRejectsNonPositiveLoginMaxFailuresAndLockout(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADMIN_AUTH_DATABASE_URL", "postgres://admin_auth_rw:pw@postgres:5432/admin_auth")
	t.Setenv("ADMIN_AUTH_SIGNING_KEY_PATH", "/run/secrets/admin_auth_signing_key.pem")
	t.Setenv("ADMIN_AUTH_LOGIN_MAX_FAILURES", "0")
	t.Setenv("ADMIN_AUTH_LOGIN_LOCKOUT", "0s")

	_, err := config.Load(config.ModeServe)
	if err == nil {
		t.Fatal("Load with a zero failure count and lockout returned no error")
	}
	for _, want := range []string{"ADMIN_AUTH_LOGIN_MAX_FAILURES", "ADMIN_AUTH_LOGIN_LOCKOUT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

func TestLoadLoginConfigFromEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADMIN_AUTH_DATABASE_URL", "postgres://admin_auth_rw:pw@postgres:5432/admin_auth")
	t.Setenv("ADMIN_AUTH_SIGNING_KEY_PATH", "/run/secrets/admin_auth_signing_key.pem")
	t.Setenv("ADMIN_AUTH_TOTP_KEY_PATH", writeTOTPKey(t))
	t.Setenv("ADMIN_AUTH_REFRESH_TOKEN_TTL", "24h")
	t.Setenv("ADMIN_AUTH_LOGIN_MAX_FAILURES", "9")
	t.Setenv("ADMIN_AUTH_LOGIN_LOCKOUT", "30m")

	cfg, err := config.Load(config.ModeServe)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RefreshTokenTTL != 24*time.Hour {
		t.Errorf("RefreshTokenTTL = %s, want 24h", cfg.RefreshTokenTTL)
	}
	if cfg.LoginMaxFailures != 9 {
		t.Errorf("LoginMaxFailures = %d, want 9", cfg.LoginMaxFailures)
	}
	if cfg.LoginLockout != 30*time.Minute {
		t.Errorf("LoginLockout = %s, want 30m", cfg.LoginLockout)
	}
}

func TestLoadRejectsOutOfRangeInviteTTL(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADMIN_AUTH_DATABASE_URL", "postgres://admin_auth_rw:pw@postgres:5432/admin_auth")
	t.Setenv("ADMIN_AUTH_SIGNING_KEY_PATH", "/run/secrets/admin_auth_signing_key.pem")

	for _, raw := range []string{"30m", "200h"} {
		t.Setenv("ADMIN_AUTH_INVITE_TTL", raw)
		_, err := config.Load(config.ModeServe)
		if err == nil || !strings.Contains(err.Error(), "ADMIN_AUTH_INVITE_TTL") {
			t.Fatalf("Load with ADMIN_AUTH_INVITE_TTL=%s: error = %v, want one naming the variable", raw, err)
		}
	}
}

func TestLoadRejectsTooLongAccessTokenTTL(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADMIN_AUTH_DATABASE_URL", "postgres://admin_auth_rw:pw@postgres:5432/admin_auth")
	t.Setenv("ADMIN_AUTH_SIGNING_KEY_PATH", "/run/secrets/admin_auth_signing_key.pem")
	t.Setenv("ADMIN_AUTH_ACCESS_TOKEN_TTL", "2h")

	_, err := config.Load(config.ModeServe)
	if err == nil || !strings.Contains(err.Error(), "ADMIN_AUTH_ACCESS_TOKEN_TTL") {
		t.Fatalf("error = %v, want one naming ADMIN_AUTH_ACCESS_TOKEN_TTL", err)
	}
}

func TestLoadServeRequiresTOTPKey(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADMIN_AUTH_DATABASE_URL", "postgres://admin_auth_rw:pw@postgres:5432/admin_auth")
	t.Setenv("ADMIN_AUTH_SIGNING_KEY_PATH", "/run/secrets/admin_auth_signing_key.pem")

	_, err := config.Load(config.ModeServe)
	if err == nil || !strings.Contains(err.Error(), "ADMIN_AUTH_TOTP_KEY_PATH") {
		t.Fatalf("error = %v, want one naming ADMIN_AUTH_TOTP_KEY_PATH", err)
	}
}

func TestLoadServeRejectsBadSizeTOTPKey(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADMIN_AUTH_DATABASE_URL", "postgres://admin_auth_rw:pw@postgres:5432/admin_auth")
	t.Setenv("ADMIN_AUTH_SIGNING_KEY_PATH", "/run/secrets/admin_auth_signing_key.pem")

	path := filepath.Join(t.TempDir(), "short.key")
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(make([]byte, 16))), 0o600); err != nil {
		t.Fatalf("write short key: %v", err)
	}
	t.Setenv("ADMIN_AUTH_TOTP_KEY_PATH", path)

	_, err := config.Load(config.ModeServe)
	if err == nil || !strings.Contains(err.Error(), "ADMIN_AUTH_TOTP_KEY_PATH") {
		t.Fatalf("error = %v, want one naming ADMIN_AUTH_TOTP_KEY_PATH", err)
	}
}

func TestLoadServeRejectsUnreadableTOTPKey(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADMIN_AUTH_DATABASE_URL", "postgres://admin_auth_rw:pw@postgres:5432/admin_auth")
	t.Setenv("ADMIN_AUTH_SIGNING_KEY_PATH", "/run/secrets/admin_auth_signing_key.pem")
	t.Setenv("ADMIN_AUTH_TOTP_KEY_PATH", filepath.Join(t.TempDir(), "does-not-exist.key"))

	_, err := config.Load(config.ModeServe)
	if err == nil || !strings.Contains(err.Error(), "ADMIN_AUTH_TOTP_KEY_PATH") {
		t.Fatalf("error = %v, want one naming ADMIN_AUTH_TOTP_KEY_PATH", err)
	}
}

func TestLoadGenkeyDoesNotRequireSigningKeyPath(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADMIN_AUTH_DATABASE_URL", "postgres://admin_auth_rw:pw@postgres:5432/admin_auth")

	cfg, err := config.Load(config.ModeGenkey)
	if err != nil {
		t.Fatalf("Load(ModeGenkey): %v", err)
	}
	if cfg.SigningKeyPath != "" {
		t.Errorf("SigningKeyPath = %q, want empty", cfg.SigningKeyPath)
	}
}

func TestLoadMigrateRequiresDatabaseURL(t *testing.T) {
	clearEnv(t)

	_, err := config.Load(config.ModeMigrate)
	if err == nil || !strings.Contains(err.Error(), "ADMIN_AUTH_DATABASE_URL") {
		t.Fatalf("error = %v, want one naming ADMIN_AUTH_DATABASE_URL", err)
	}
}

func TestLoadBootstrapRootNeedsOnlyDatabaseURL(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADMIN_AUTH_DATABASE_URL", "postgres://admin_auth_rw:pw@postgres:5432/admin_auth")

	cfg, err := config.Load(config.ModeBootstrapRoot)
	if err != nil {
		t.Fatalf("Load(ModeBootstrapRoot): %v", err)
	}
	if cfg.SigningKeyPath != "" {
		t.Errorf("SigningKeyPath = %q, want empty", cfg.SigningKeyPath)
	}
	if cfg.RootPasswordFile != "/run/secrets/admin_root_password" {
		t.Errorf("RootPasswordFile = %q, want the default", cfg.RootPasswordFile)
	}
}
