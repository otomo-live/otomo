package config

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"
)

// setMinimalPlayerEnv sets the minimum env vars for a valid configuration.
//
// The staff vars are part of the minimum, not optional extras: two routes in
// this service's table (the dev and staging manifests) take a staff token, so
// this gateway loads a second issuer. Do not "clean these up".
func setMinimalPlayerEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GATEWAY_LISTEN_ADDR", ":8080")
	t.Setenv("GATEWAY_METRICS_ADDR", ":9090")
	t.Setenv("GATEWAY_PLAYER_JWKS_URL", "http://auth:8080/.well-known/jwks.json")
	t.Setenv("GATEWAY_PLAYER_ISSUER", "https://auth.otomo.internal")
	t.Setenv("GATEWAY_PLAYER_AUDIENCE", "otomo:player")
	t.Setenv("GATEWAY_STAFF_JWKS_URL", "http://php-admin:8080/.well-known/jwks.json")
	t.Setenv("GATEWAY_STAFF_ISSUER", "https://admin-auth.otomo.internal")
	t.Setenv("GATEWAY_STAFF_AUDIENCE", "otomo:staff")
}

func TestLoad_ValidConfigs(t *testing.T) {
	t.Run("minimal", func(t *testing.T) {
		setMinimalPlayerEnv(t)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.PlayerIssuer != "https://auth.otomo.internal" {
			t.Errorf("PlayerIssuer = %q, want the player issuer", cfg.PlayerIssuer)
		}
		if cfg.ReadTimeout != 10_000_000_000 {
			t.Errorf("ReadTimeout = %v, want 10s default", cfg.ReadTimeout)
		}
	})

	// GATEWAY_INSTANCE is gone: this binary is the player gateway. Setting it
	// must not change anything, and in particular must not select another table.
	t.Run("GATEWAY_INSTANCE is inert", func(t *testing.T) {
		setMinimalPlayerEnv(t)
		t.Setenv("GATEWAY_INSTANCE", "admin")
		if _, err := Load(); err != nil {
			t.Fatalf("GATEWAY_INSTANCE should not be read at all: %v", err)
		}
	})

	t.Run("upstreams parsed and normalised", func(t *testing.T) {
		setMinimalPlayerEnv(t)
		t.Setenv("GATEWAY_UPSTREAM_SESSION_URL", "http://session:8080")
		t.Setenv("GATEWAY_UPSTREAM_PATCH_URL", "http://patch:8080")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if u := cfg.Upstreams["session"]; u == nil || u.Host != "session:8080" {
			t.Errorf("upstream session = %v, want http://session:8080", u)
		}
		if u := cfg.Upstreams["patch"]; u == nil || u.Host != "patch:8080" {
			t.Errorf("upstream patch = %v, want http://patch:8080", u)
		}
	})

	// The GATEWAY_DEV_UPSTREAM_ form belongs to services/gateway_dev. Both
	// services reach the same Session deployment under different prefixes, so a
	// shared env file must not be able to cross the two keys over.
	t.Run("GATEWAY_DEV_ upstream vars are ignored", func(t *testing.T) {
		setMinimalPlayerEnv(t)
		t.Setenv("GATEWAY_DEV_UPSTREAM_SESSION_URL", "http://staff-side-session:8080")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := cfg.Upstreams["session"]; ok {
			t.Errorf("GATEWAY_DEV_UPSTREAM_SESSION_URL was read as an upstream: %v", cfg.Upstreams)
		}
	})

	t.Run("custom timeouts", func(t *testing.T) {
		setMinimalPlayerEnv(t)
		t.Setenv("GATEWAY_READ_TIMEOUT", "5s")
		t.Setenv("GATEWAY_WRITE_TIMEOUT", "60s")
		t.Setenv("GATEWAY_IDLE_TIMEOUT", "90s")
		t.Setenv("GATEWAY_JWT_CLOCK_SKEW", "15s")
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
		setMinimalPlayerEnv(t)
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

	// GATEWAY_TRUSTED_PROXIES: a comma-separated list of CIDRs or bare IPs. A
	// bare IP becomes a /32 or /128, CIDRs are masked, and empty means trust
	// nobody (today's behaviour). The proxy middleware itself is tested in
	// internal/clientip.
	t.Run("trusted proxies", func(t *testing.T) {
		tests := []struct {
			name string
			env  string
			want []netip.Prefix
		}{
			{"empty trusts nobody", "", nil},
			{"single bare IP becomes a /32", "10.0.0.7", []netip.Prefix{netip.MustParsePrefix("10.0.0.7/32")}},
			{"CIDR list with whitespace", " 10.0.0.0/8 , 192.168.0.0/16 ", []netip.Prefix{
				netip.MustParsePrefix("10.0.0.0/8"),
				netip.MustParsePrefix("192.168.0.0/16"),
			}},
			{"CIDR host bits are masked", "10.1.2.3/8", []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}},
			{"IPv6 bare IP and CIDR", "2001:db8::1, fd00::/8", []netip.Prefix{
				netip.MustParsePrefix("2001:db8::1/128"),
				netip.MustParsePrefix("fd00::/8"),
			}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				setMinimalPlayerEnv(t)
				t.Setenv("GATEWAY_TRUSTED_PROXIES", tt.env)
				cfg, err := Load()
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if !slices.Equal(cfg.TrustedProxies, tt.want) {
					t.Errorf("TrustedProxies = %v, want %v", cfg.TrustedProxies, tt.want)
				}
			})
		}
	})

	t.Run("rate limit overrides", func(t *testing.T) {
		setMinimalPlayerEnv(t)
		t.Setenv("GATEWAY_RATE_LIMIT_RPS", "50")
		t.Setenv("GATEWAY_RATE_LIMIT_BURST", "100")
		t.Setenv("GATEWAY_LOGIN_RATE_LIMIT_RPS", "3")
		t.Setenv("GATEWAY_LOGIN_RATE_LIMIT_BURST", "6")
		t.Setenv("GATEWAY_SWEEP_INTERVAL", "2m")
		t.Setenv("GATEWAY_MAX_IDLE_AGE", "8m")
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
			name:  "missing GATEWAY_LISTEN_ADDR",
			setup: setMinimalPlayerEnv,
			unset: "GATEWAY_LISTEN_ADDR",
		},
		{
			name:  "missing GATEWAY_METRICS_ADDR",
			setup: setMinimalPlayerEnv,
			unset: "GATEWAY_METRICS_ADDR",
		},
		{
			name:  "missing GATEWAY_STAFF_JWKS_URL",
			setup: setMinimalPlayerEnv,
			unset: "GATEWAY_STAFF_JWKS_URL",
		},
		{
			name:  "missing GATEWAY_STAFF_ISSUER",
			setup: setMinimalPlayerEnv,
			unset: "GATEWAY_STAFF_ISSUER",
		},
		{
			name:  "missing GATEWAY_STAFF_AUDIENCE",
			setup: setMinimalPlayerEnv,
			unset: "GATEWAY_STAFF_AUDIENCE",
		},
		{
			name:  "missing GATEWAY_PLAYER_JWKS_URL",
			setup: setMinimalPlayerEnv,
			unset: "GATEWAY_PLAYER_JWKS_URL",
		},
		{
			name:  "missing GATEWAY_PLAYER_ISSUER",
			setup: setMinimalPlayerEnv,
			unset: "GATEWAY_PLAYER_ISSUER",
		},
		{
			name:  "missing GATEWAY_PLAYER_AUDIENCE",
			setup: setMinimalPlayerEnv,
			unset: "GATEWAY_PLAYER_AUDIENCE",
		},
		{
			name: "relative GATEWAY_PLAYER_JWKS_URL",
			setup: func(t *testing.T) {
				setMinimalPlayerEnv(t)
				t.Setenv("GATEWAY_PLAYER_JWKS_URL", "/.well-known/jwks.json")
			},
			wantErr: "GATEWAY_PLAYER_JWKS_URL",
		},
		{
			name: "relative GATEWAY_STAFF_JWKS_URL",
			setup: func(t *testing.T) {
				setMinimalPlayerEnv(t)
				t.Setenv("GATEWAY_STAFF_JWKS_URL", "/.well-known/jwks.json")
			},
			wantErr: "GATEWAY_STAFF_JWKS_URL",
		},
		{
			name: "TLS cert without key",
			setup: func(t *testing.T) {
				setMinimalPlayerEnv(t)
				t.Setenv("GATEWAY_TLS_CERT_FILE", "/path/to/cert.pem")
				t.Setenv("GATEWAY_TLS_KEY_FILE", "")
			},
			wantErr: "GATEWAY_TLS_CERT_FILE",
		},
		{
			name: "TLS key without cert",
			setup: func(t *testing.T) {
				setMinimalPlayerEnv(t)
				t.Setenv("GATEWAY_TLS_CERT_FILE", "")
				t.Setenv("GATEWAY_TLS_KEY_FILE", "/path/to/key.pem")
			},
			wantErr: "GATEWAY_TLS_KEY_FILE",
		},
		{
			name: "invalid upstream URL",
			setup: func(t *testing.T) {
				setMinimalPlayerEnv(t)
				t.Setenv("GATEWAY_UPSTREAM_SESSION_URL", "://bad")
			},
			wantErr: "GATEWAY_UPSTREAM_SESSION_URL",
		},
		{
			name: "upstream URL with no scheme",
			setup: func(t *testing.T) {
				setMinimalPlayerEnv(t)
				t.Setenv("GATEWAY_UPSTREAM_SESSION_URL", "session:8080")
			},
			wantErr: "GATEWAY_UPSTREAM_SESSION_URL",
		},
		{
			name: "invalid read timeout",
			setup: func(t *testing.T) {
				setMinimalPlayerEnv(t)
				t.Setenv("GATEWAY_READ_TIMEOUT", "not-a-duration")
			},
			wantErr: "GATEWAY_READ_TIMEOUT",
		},
		{
			name: "invalid trusted proxy IP",
			setup: func(t *testing.T) {
				setMinimalPlayerEnv(t)
				t.Setenv("GATEWAY_TRUSTED_PROXIES", "10.0.0.0/8,not-an-ip")
			},
			wantErr: "not-an-ip",
		},
		{
			name: "invalid trusted proxy CIDR",
			setup: func(t *testing.T) {
				setMinimalPlayerEnv(t)
				t.Setenv("GATEWAY_TRUSTED_PROXIES", "10.0.0.0/99")
			},
			wantErr: "10.0.0.0/99",
		},
		{
			name: "invalid rate limit RPS",
			setup: func(t *testing.T) {
				setMinimalPlayerEnv(t)
				t.Setenv("GATEWAY_RATE_LIMIT_RPS", "abc")
			},
			wantErr: "GATEWAY_RATE_LIMIT_RPS",
		},
	}

	// Rate limiting (techspec §6.4): every tunable has a rejection case.
	for _, c := range []struct{ key, value string }{
		{"GATEWAY_RATE_LIMIT_RPS", "0"},
		{"GATEWAY_RATE_LIMIT_BURST", "-1"},
		{"GATEWAY_LOGIN_RATE_LIMIT_RPS", "0"},
		{"GATEWAY_LOGIN_RATE_LIMIT_BURST", "0"},
		{"GATEWAY_LOGIN_RATE_LIMIT_RPS", "five"},
		{"GATEWAY_SWEEP_INTERVAL", "0s"},
		{"GATEWAY_SWEEP_INTERVAL", "soon"},
		{"GATEWAY_MAX_IDLE_AGE", "-1m"},
	} {
		tests = append(tests, struct {
			name    string
			setup   func(t *testing.T)
			unset   string
			wantErr string
		}{
			name: c.key + "=" + c.value + " rejected",
			setup: func(t *testing.T) {
				setMinimalPlayerEnv(t)
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
