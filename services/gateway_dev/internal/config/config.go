package config

import (
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds every env var this service reads, parsed once at startup.
// Nothing reads the environment after Load returns.
//
// There is no Instance field and no GATEWAY_INSTANCE. Which route table this
// process loads is not a runtime decision: it is this binary. That is the whole
// reason the service exists (see the README), and it means the validator cannot
// disagree with the route table about what is required.
type Config struct {
	ListenAddr  string
	MetricsAddr string

	TLSCertFile string
	TLSKeyFile  string

	// TrustedProxies are the peers whose X-Forwarded-For is believed, from
	// GATEWAY_DEV_TRUSTED_PROXIES (comma-separated CIDRs or bare IPs). The staff
	// gateway is loopback-only with nothing in front of it, so this is empty in
	// every deployment today; it exists because the server code is shared with
	// services/gateway byte for byte (ci/scripts/shared-package-drift.sh).
	TrustedProxies []netip.Prefix

	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration
	JWTClockSkew time.Duration

	// The staff domain is the only identity domain here, and the only one this
	// process can be configured with: admin-auth issues staff tokens
	// (design/06-auth-identity-contract.md §5). The names carry no GATEWAY_DEV_
	// prefix because the contract fixes them and services/gateway uses the same
	// three for the two dev/staging manifest routes that cross into this domain.
	StaffJWKSURL  string
	StaffIssuer   string
	StaffAudience string

	// Upstreams are parsed from GATEWAY_DEV_UPSTREAM_<NAME>_URL env vars.
	// Name normalisation: GATEWAY_DEV_UPSTREAM_ADMINAUTH_URL → key "adminauth",
	// matching the Upstream values in techspec §5.2.
	Upstreams map[string]*url.URL

	// Per-IP rate limiting (techspec §6.4). The general limit covers every
	// route; the login limit is the stricter override for /admin-auth/, the
	// brute-force target. The sweeper evicts idle per-IP entries so the map
	// cannot grow without bound.
	RateLimitRPS        int
	RateLimitBurst      int
	LoginRateLimitRPS   int
	LoginRateLimitBurst int
	SweepInterval       time.Duration
	MaxIdleAge          time.Duration
}

// Load parses all GATEWAY_DEV_* and GATEWAY_STAFF_* env vars into a Config and
// returns an error if any required var is missing or malformed.
func Load() (*Config, error) {
	cfg := &Config{
		ListenAddr:    os.Getenv("GATEWAY_DEV_LISTEN_ADDR"),
		MetricsAddr:   os.Getenv("GATEWAY_DEV_METRICS_ADDR"),
		TLSCertFile:   os.Getenv("GATEWAY_DEV_TLS_CERT_FILE"),
		TLSKeyFile:    os.Getenv("GATEWAY_DEV_TLS_KEY_FILE"),
		StaffJWKSURL:  os.Getenv("GATEWAY_STAFF_JWKS_URL"),
		StaffIssuer:   os.Getenv("GATEWAY_STAFF_ISSUER"),
		StaffAudience: os.Getenv("GATEWAY_STAFF_AUDIENCE"),
	}

	var err error

	cfg.ReadTimeout, err = durationEnv("GATEWAY_DEV_READ_TIMEOUT", 10*time.Second)
	if err != nil {
		return nil, err
	}
	cfg.WriteTimeout, err = durationEnv("GATEWAY_DEV_WRITE_TIMEOUT", 30*time.Second)
	if err != nil {
		return nil, err
	}
	cfg.IdleTimeout, err = durationEnv("GATEWAY_DEV_IDLE_TIMEOUT", 120*time.Second)
	if err != nil {
		return nil, err
	}
	cfg.TrustedProxies, err = parseTrustedProxies(os.Getenv("GATEWAY_DEV_TRUSTED_PROXIES"))
	if err != nil {
		return nil, err
	}
	cfg.JWTClockSkew, err = durationEnv("GATEWAY_DEV_JWT_CLOCK_SKEW", 30*time.Second)
	if err != nil {
		return nil, err
	}

	cfg.RateLimitRPS, err = intEnv("GATEWAY_DEV_RATE_LIMIT_RPS", 20)
	if err != nil {
		return nil, err
	}
	cfg.RateLimitBurst, err = intEnv("GATEWAY_DEV_RATE_LIMIT_BURST", 40)
	if err != nil {
		return nil, err
	}

	// Defaults: techspec §6.4 login override ("e.g. 5 rps / burst 10").
	cfg.LoginRateLimitRPS, err = intEnv("GATEWAY_DEV_LOGIN_RATE_LIMIT_RPS", 5)
	if err != nil {
		return nil, err
	}
	cfg.LoginRateLimitBurst, err = intEnv("GATEWAY_DEV_LOGIN_RATE_LIMIT_BURST", 10)
	if err != nil {
		return nil, err
	}

	// Defaults: techspec §6.4 ("every 5 min, evict entries idle longer than 10 min").
	cfg.SweepInterval, err = durationEnv("GATEWAY_DEV_SWEEP_INTERVAL", 5*time.Minute)
	if err != nil {
		return nil, err
	}
	cfg.MaxIdleAge, err = durationEnv("GATEWAY_DEV_MAX_IDLE_AGE", 10*time.Minute)
	if err != nil {
		return nil, err
	}

	cfg.Upstreams, err = parseUpstreams()
	if err != nil {
		return nil, err
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func (c *Config) validate() error {
	if c.ListenAddr == "" {
		return fmt.Errorf("GATEWAY_DEV_LISTEN_ADDR is required")
	}
	if c.MetricsAddr == "" {
		return fmt.Errorf("GATEWAY_DEV_METRICS_ADDR is required")
	}

	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return fmt.Errorf("GATEWAY_DEV_TLS_CERT_FILE and GATEWAY_DEV_TLS_KEY_FILE must both be set or both empty")
	}

	if c.StaffJWKSURL == "" {
		return fmt.Errorf("GATEWAY_STAFF_JWKS_URL is required")
	}
	if c.StaffIssuer == "" {
		return fmt.Errorf("GATEWAY_STAFF_ISSUER is required")
	}
	if c.StaffAudience == "" {
		return fmt.Errorf("GATEWAY_STAFF_AUDIENCE is required")
	}

	// Validated here, not at first fetch: a JWKS URL with a typo in it is
	// indistinguishable from admin-auth being down once the process is
	// running, and the two have very different fixes.
	if c.StaffJWKSURL != "" {
		parsed, err := url.Parse(c.StaffJWKSURL)
		if err != nil {
			return fmt.Errorf("GATEWAY_STAFF_JWKS_URL: %w", err)
		}
		if parsed.Scheme == "" || parsed.Host == "" {
			return fmt.Errorf("GATEWAY_STAFF_JWKS_URL: must be an absolute URL, got %q", c.StaffJWKSURL)
		}
	}

	// A zero RPS with a positive burst admits the burst and then 429s every
	// request from that IP forever; a zero burst rejects everything. Neither is
	// a usable limit, so all four must be positive (techspec §6.4).
	for _, l := range []struct {
		key string
		val int
	}{
		{"GATEWAY_DEV_RATE_LIMIT_RPS", c.RateLimitRPS},
		{"GATEWAY_DEV_RATE_LIMIT_BURST", c.RateLimitBurst},
		{"GATEWAY_DEV_LOGIN_RATE_LIMIT_RPS", c.LoginRateLimitRPS},
		{"GATEWAY_DEV_LOGIN_RATE_LIMIT_BURST", c.LoginRateLimitBurst},
	} {
		if l.val <= 0 {
			return fmt.Errorf("%s must be positive, got %d", l.key, l.val)
		}
	}
	// A zero ticker interval panics in time.NewTicker; a zero idle age would
	// evict every entry on each sweep and reset every client's bucket.
	if c.SweepInterval <= 0 {
		return fmt.Errorf("GATEWAY_DEV_SWEEP_INTERVAL must be positive, got %v", c.SweepInterval)
	}
	if c.MaxIdleAge <= 0 {
		return fmt.Errorf("GATEWAY_DEV_MAX_IDLE_AGE must be positive, got %v", c.MaxIdleAge)
	}

	return nil
}

// parseUpstreams scans env for GATEWAY_DEV_UPSTREAM_<NAME>_URL vars.
// Names are normalised to lowercase: GATEWAY_DEV_UPSTREAM_ADMINAUTH_URL → "adminauth".
func parseUpstreams() (map[string]*url.URL, error) {
	upstreams := make(map[string]*url.URL)
	for _, env := range os.Environ() {
		k, v, ok := strings.Cut(env, "=")
		if !ok {
			continue
		}
		if !strings.HasPrefix(k, "GATEWAY_DEV_UPSTREAM_") || !strings.HasSuffix(k, "_URL") {
			continue
		}
		name := strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(k, "GATEWAY_DEV_UPSTREAM_"), "_URL"))
		if name == "" {
			continue
		}
		u, err := url.Parse(v)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("%s: invalid URL %q", k, v)
		}
		upstreams[name] = u
	}
	return upstreams, nil
}

func durationEnv(key string, def time.Duration) (time.Duration, error) {
	s := os.Getenv(key)
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration %q: %w", key, s, err)
	}
	return d, nil
}

func intEnv(key string, def int) (int, error) {
	s := os.Getenv(key)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid integer %q: %w", key, s, err)
	}
	return n, nil
}

// parseTrustedProxies parses GATEWAY_DEV_TRUSTED_PROXIES: a comma-separated list of
// CIDRs or bare IPs, whitespace-tolerant. A bare IP becomes a /32 or /128, so
// one setting accepts both "10.0.0.7" and "10.0.0.0/24". CIDRs are masked, so
// "10.0.0.1/8" behaves as "10.0.0.0/8" instead of silently matching nothing.
func parseTrustedProxies(raw string) ([]netip.Prefix, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var prefixes []netip.Prefix
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			p, err := netip.ParsePrefix(entry)
			if err != nil {
				return nil, fmt.Errorf("GATEWAY_DEV_TRUSTED_PROXIES: invalid entry %q: %w", entry, err)
			}
			prefixes = append(prefixes, p.Masked())
			continue
		}
		addr, err := netip.ParseAddr(entry)
		if err != nil {
			return nil, fmt.Errorf("GATEWAY_DEV_TRUSTED_PROXIES: invalid entry %q: %w", entry, err)
		}
		addr = addr.Unmap()
		prefixes = append(prefixes, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return prefixes, nil
}
