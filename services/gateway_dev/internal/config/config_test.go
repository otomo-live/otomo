package config

import (
	"strings"
	"testing"
	"time"
)

// setMinimalEnv sets the minimum env vars for a valid configuration. There is no
// instance here: this binary is the staff edge, so the staff domain is the only
// domain and the only one Load can be given.
func setMinimalEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GATEWAY_DEV_LISTEN_ADDR", ":8080")
	t.Setenv("GATEWAY_DEV_METRICS_ADDR", ":9090")
	t.Setenv("GATEWAY_STAFF_JWKS_URL", "http://admin-auth:8080/.well-known/jwks.json")
	t.Setenv("GATEWAY_STAFF_ISSUER", "https://admin-auth.otomo.internal")
	t.Setenv("GATEWAY_STAFF_AUDIENCE", "otomo:staff")
}

func TestLoad_ValidConfigs(t *testing.T) {
	t.Run("minimal", func(t *testing.T) {
		setMinimalEnv(t)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.ListenAddr != ":8080" {
			t.Errorf("ListenAddr = %q, want :8080", cfg.ListenAddr)
		}
		if cfg.ReadTimeout != 10_000_000_000 {
			t.Errorf("ReadTimeout = %v, want 10s default", cfg.ReadTimeout)
		}
	})

	// The relocated "admin does not require player JWKS" case, inverted: there is
	// no player field to require, so setting the old player vars must change
	// nothing at all. If someone reintroduces them, this fails.
	t.Run("player env vars are inert", func(t *testing.T) {
		setMinimalEnv(t)
		t.Setenv("GATEWAY_PLAYER_JWKS_URL", "http://auth:8080/.well-known/jwks.json")
		t.Setenv("GATEWAY_PLAYER_ISSUER", "https://auth.otomo.internal")
		t.Setenv("GATEWAY_PLAYER_AUDIENCE", "otomo:player")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("player vars should not be read at all: %v", err)
		}
		if cfg.StaffIssuer != "https://admin-auth.otomo.internal" {
			t.Errorf("StaffIssuer = %q, want the staff issuer", cfg.StaffIssuer)
		}
	})

	t.Run("upstreams parsed and normalised", func(t *testing.T) {
		setMinimalEnv(t)
		t.Setenv("GATEWAY_DEV_UPSTREAM_SESSION_URL", "http://session:8080")
		t.Setenv("GATEWAY_DEV_UPSTREAM_ADMINAUTH_URL", "http://admin-auth:8080")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if u := cfg.Upstreams["session"]; u == nil || u.Host != "session:8080" {
			t.Errorf("upstream session = %v, want http://session:8080", u)
		}
		if u := cfg.Upstreams["adminauth"]; u == nil || u.Host != "admin-auth:8080" {
			t.Errorf("upstream adminauth = %v, want http://admin-auth:8080", u)
		}
	})

	// The un-prefixed GATEWAY_UPSTREAM_ form belongs to services/gateway. Both
	// services read the same Session deployment, so a shared env file must not be
	// able to cross the two keys over.
	t.Run("unprefixed upstream vars are ignored", func(t *testing.T) {
		setMinimalEnv(t)
		t.Setenv("GATEWAY_UPSTREAM_SESSION_URL", "http://player-side-session:8080")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := cfg.Upstreams["session"]; ok {
			t.Errorf("GATEWAY_UPSTREAM_SESSION_URL was read as an upstream: %v", cfg.Upstreams)
		}
	})

	t.Run("custom timeouts", func(t *testing.T) {
		setMinimalEnv(t)
		t.Setenv("GATEWAY_DEV_READ_TIMEOUT", "5s")
		t.Setenv("GATEWAY_DEV_WRITE_TIMEOUT", "60s")
		t.Setenv("GATEWAY_DEV_IDLE_TIMEOUT", "90s")
		t.Setenv("GATEWAY_DEV_JWT_CLOCK_SKEW", "15s")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.ReadTimeout != 5_000_000_000 {
			t.Errorf("ReadTimeout = %v, want 5s", cfg.ReadTimeout)
		}
		if cfg.WriteTimeout != 60_000_000_000 {
			t.Errorf("WriteTimeout = %v, want 60s", cfg.WriteTimeout)
		}
	})

	t.Run("rate limit defaults (techspec §3, §6.4)", func(t *testing.T) {
		setMinimalEnv(t)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		checks := []struct {
			name string
			got  any
			want any
		}{
			{"RateLimitRPS", cfg.RateLimitRPS, 20},
			{"RateLimitBurst", cfg.RateLimitBurst, 40},
			{"LoginRateLimitRPS", cfg.LoginRateLimitRPS, 5},
			{"LoginRateLimitBurst", cfg.LoginRateLimitBurst, 10},
			{"SweepInterval", cfg.SweepInterval, 5 * time.Minute},
			{"MaxIdleAge", cfg.MaxIdleAge, 10 * time.Minute},
		}
		for _, c := range checks {
			if c.got != c.want {
				t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
			}
		}
	})

	t.Run("rate limit overrides", func(t *testing.T) {
		setMinimalEnv(t)
		t.Setenv("GATEWAY_DEV_RATE_LIMIT_RPS", "50")
		t.Setenv("GATEWAY_DEV_RATE_LIMIT_BURST", "100")
		t.Setenv("GATEWAY_DEV_LOGIN_RATE_LIMIT_RPS", "3")
		t.Setenv("GATEWAY_DEV_LOGIN_RATE_LIMIT_BURST", "6")
		t.Setenv("GATEWAY_DEV_SWEEP_INTERVAL", "2m")
		t.Setenv("GATEWAY_DEV_MAX_IDLE_AGE", "8m")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		checks := []struct {
			name string
			got  any
			want any
		}{
			{"RateLimitRPS", cfg.RateLimitRPS, 50},
			{"RateLimitBurst", cfg.RateLimitBurst, 100},
			{"LoginRateLimitRPS", cfg.LoginRateLimitRPS, 3},
			{"LoginRateLimitBurst", cfg.LoginRateLimitBurst, 6},
			{"SweepInterval", cfg.SweepInterval, 2 * time.Minute},
			{"MaxIdleAge", cfg.MaxIdleAge, 8 * time.Minute},
		}
		for _, c := range checks {
			if c.got != c.want {
				t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
			}
		}
	})
}

func TestLoad_IncompleteEnvFails(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T)
		unset   string
		wantErr string
	}{
		{
			name:  "missing GATEWAY_DEV_LISTEN_ADDR",
			setup: setMinimalEnv,
			unset: "GATEWAY_DEV_LISTEN_ADDR",
		},
		{
			name:  "missing GATEWAY_DEV_METRICS_ADDR",
			setup: setMinimalEnv,
			unset: "GATEWAY_DEV_METRICS_ADDR",
		},
		{
			name:  "missing GATEWAY_STAFF_JWKS_URL",
			setup: setMinimalEnv,
			unset: "GATEWAY_STAFF_JWKS_URL",
		},
		{
			name:  "missing GATEWAY_STAFF_ISSUER",
			setup: setMinimalEnv,
			unset: "GATEWAY_STAFF_ISSUER",
		},
		{
			name:  "missing GATEWAY_STAFF_AUDIENCE",
			setup: setMinimalEnv,
			unset: "GATEWAY_STAFF_AUDIENCE",
		},
		{
			// Checked at boot rather than at first fetch: once the process is
			// running, a typo here looks exactly like admin-auth being down.
			name: "relative GATEWAY_STAFF_JWKS_URL",
			setup: func(t *testing.T) {
				setMinimalEnv(t)
				t.Setenv("GATEWAY_STAFF_JWKS_URL", "/.well-known/jwks.json")
			},
			wantErr: "GATEWAY_STAFF_JWKS_URL",
		},
		{
			name: "TLS cert without key",
			setup: func(t *testing.T) {
				setMinimalEnv(t)
				t.Setenv("GATEWAY_DEV_TLS_CERT_FILE", "/path/to/cert.pem")
				t.Setenv("GATEWAY_DEV_TLS_KEY_FILE", "")
			},
			wantErr: "GATEWAY_DEV_TLS_CERT_FILE",
		},
		{
			name: "TLS key without cert",
			setup: func(t *testing.T) {
				setMinimalEnv(t)
				t.Setenv("GATEWAY_DEV_TLS_CERT_FILE", "")
				t.Setenv("GATEWAY_DEV_TLS_KEY_FILE", "/path/to/key.pem")
			},
			wantErr: "GATEWAY_DEV_TLS_KEY_FILE",
		},
		{
			name: "invalid upstream URL",
			setup: func(t *testing.T) {
				setMinimalEnv(t)
				t.Setenv("GATEWAY_DEV_UPSTREAM_SESSION_URL", "://bad")
			},
			wantErr: "GATEWAY_DEV_UPSTREAM_SESSION_URL",
		},
		{
			name: "upstream URL with no scheme",
			setup: func(t *testing.T) {
				setMinimalEnv(t)
				t.Setenv("GATEWAY_DEV_UPSTREAM_SESSION_URL", "session:8080")
			},
			wantErr: "GATEWAY_DEV_UPSTREAM_SESSION_URL",
		},
		{
			name: "invalid read timeout",
			setup: func(t *testing.T) {
				setMinimalEnv(t)
				t.Setenv("GATEWAY_DEV_READ_TIMEOUT", "not-a-duration")
			},
			wantErr: "GATEWAY_DEV_READ_TIMEOUT",
		},
		{
			name: "invalid rate limit RPS",
			setup: func(t *testing.T) {
				setMinimalEnv(t)
				t.Setenv("GATEWAY_DEV_RATE_LIMIT_RPS", "abc")
			},
			wantErr: "GATEWAY_DEV_RATE_LIMIT_RPS",
		},
	}

	// Rate limiting (techspec §6.4): every tunable has a rejection case.
	for _, c := range []struct{ key, value string }{
		{"GATEWAY_DEV_RATE_LIMIT_RPS", "0"},
		{"GATEWAY_DEV_RATE_LIMIT_BURST", "-1"},
		{"GATEWAY_DEV_LOGIN_RATE_LIMIT_RPS", "0"},
		{"GATEWAY_DEV_LOGIN_RATE_LIMIT_BURST", "0"},
		{"GATEWAY_DEV_LOGIN_RATE_LIMIT_RPS", "five"},
		{"GATEWAY_DEV_SWEEP_INTERVAL", "0s"},
		{"GATEWAY_DEV_SWEEP_INTERVAL", "soon"},
		{"GATEWAY_DEV_MAX_IDLE_AGE", "-1m"},
	} {
		tests = append(tests, struct {
			name    string
			setup   func(t *testing.T)
			unset   string
			wantErr string
		}{
			name: c.key + "=" + c.value + " rejected",
			setup: func(t *testing.T) {
				setMinimalEnv(t)
				t.Setenv(c.key, c.value)
			},
			wantErr: c.key,
		})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setup(t)
			if tt.unset != "" {
				t.Setenv(tt.unset, "")
			}
			_, err := Load()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			wantStr := tt.wantErr
			if wantStr == "" {
				wantStr = tt.unset
			}
			if wantStr != "" && !strings.Contains(err.Error(), wantStr) {
				t.Errorf("error %q should mention %q", err, wantStr)
			}
		})
	}
}
