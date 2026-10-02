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

// Config holds every env var from techspec §3, parsed once at startup.
// Nothing reads the environment after Load returns.
//
// There is no Instance field and no GATEWAY_INSTANCE. This binary serves
// players; the admin and dev edge is services/gateway_dev. The route table is
// not a runtime choice, so the validator cannot disagree with it.
type Config struct {
	ListenAddr  string
	MetricsAddr string

	TLSCertFile string
	TLSKeyFile  string

	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration
	JWTClockSkew time.Duration

	// TrustedProxies are the peers whose X-Forwarded-For is believed. Parsed
	// from GATEWAY_TRUSTED_PROXIES: a comma-separated list of CIDRs or bare IPs
	// (a bare IP becomes a /32 or /128). Empty means trust nobody, so the peer
	// address is always the client; that is the correct setting when no reverse
	// proxy terminates TLS in front of this gateway.
	TrustedProxies []netip.Prefix

	// The player domain is what this service exists for. The staff domain is
	// here for exactly two routes: GET /patch/v1/dev/manifest and
	// GET /patch/v1/staging/manifest, which carry unshipped builds and so take a
	// staff token (design/03-patch-minimal.md PAT-B6). Both domains are required
	// and both are loaded, which means /readyz waits on both: a PHP Admin Auth
	// outage holds this gateway at 503 even though every player route could
	// still serve. That is an accepted trade for keeping a dev-channel client
	// pointed at one base URL, and it is the only reason a second issuer is here
	// at all. The admin routes that used to justify a second route table are not
	// in this binary; they are services/gateway_dev.
	PlayerJWKSURL  string
	PlayerIssuer   string
	PlayerAudience string

	// The same three names services/gateway_dev reads for its only domain.
	// One domain, one set of names, two consumers.
	StaffJWKSURL  string
	StaffIssuer   string
	StaffAudience string

	// Upstreams are parsed from GATEWAY_UPSTREAM_<NAME>_URL env vars.
	// Name normalisation: GATEWAY_UPSTREAM_PHPADMIN_URL → key "phpadmin",
	// matching the Upstream values in techspec §5.2.
	Upstreams map[string]*url.URL

	// Per-IP rate limiting (techspec §6.4). The general limit covers every
	// route; the login limit is the stricter override for /auth/*, the
	// brute-force target. The sweeper evicts idle per-IP entries so the map
	// cannot grow without bound.
	RateLimitRPS        int
	RateLimitBurst      int
	LoginRateLimitRPS   int
	LoginRateLimitBurst int
	SweepInterval       time.Duration
	MaxIdleAge          time.Duration
}

// Load parses all GATEWAY_* env vars into a Config and returns an error if any
// required var is missing or malformed.
func Load() (*Config, error) {
	cfg := &Config{
		ListenAddr:     os.Getenv("GATEWAY_LISTEN_ADDR"),
		MetricsAddr:    os.Getenv("GATEWAY_METRICS_ADDR"),
		TLSCertFile:    os.Getenv("GATEWAY_TLS_CERT_FILE"),
		TLSKeyFile:     os.Getenv("GATEWAY_TLS_KEY_FILE"),
		PlayerJWKSURL:  os.Getenv("GATEWAY_PLAYER_JWKS_URL"),
		PlayerIssuer:   os.Getenv("GATEWAY_PLAYER_ISSUER"),
		PlayerAudience: os.Getenv("GATEWAY_PLAYER_AUDIENCE"),
		StaffJWKSURL:   os.Getenv("GATEWAY_STAFF_JWKS_URL"),
		StaffIssuer:    os.Getenv("GATEWAY_STAFF_ISSUER"),
		StaffAudience:  os.Getenv("GATEWAY_STAFF_AUDIENCE"),
	}

	var err error

	cfg.ReadTimeout, err = durationEnv("GATEWAY_READ_TIMEOUT", 10*time.Second)
	if err != nil {
		return nil, err
	}
	cfg.WriteTimeout, err = durationEnv("GATEWAY_WRITE_TIMEOUT", 30*time.Second)
	if err != nil {
		return nil, err
	}
	cfg.IdleTimeout, err = durationEnv("GATEWAY_IDLE_TIMEOUT", 120*time.Second)
	if err != nil {
		return nil, err
	}
	cfg.JWTClockSkew, err = durationEnv("GATEWAY_JWT_CLOCK_SKEW", 30*time.Second)
	if err != nil {
		return nil, err
	}

	cfg.TrustedProxies, err = parseTrustedProxies(os.Getenv("GATEWAY_TRUSTED_PROXIES"))
	if err != nil {
		return nil, err
	}

	// Defaults: techspec §3 example column (20 rps, burst 40).
	cfg.RateLimitRPS, err = intEnv("GATEWAY_RATE_LIMIT_RPS", 20)
	if err != nil {
		return nil, err
	}
	cfg.RateLimitBurst, err = intEnv("GATEWAY_RATE_LIMIT_BURST", 40)
	if err != nil {
		return nil, err
	}

	// Defaults: techspec §6.4 login override ("e.g. 5 rps / burst 10").
	cfg.LoginRateLimitRPS, err = intEnv("GATEWAY_LOGIN_RATE_LIMIT_RPS", 5)
	if err != nil {
		return nil, err
	}
	cfg.LoginRateLimitBurst, err = intEnv("GATEWAY_LOGIN_RATE_LIMIT_BURST", 10)
	if err != nil {
		return nil, err
	}

	// Defaults: techspec §6.4 ("every 5 min, evict entries idle longer than 10 min").
	cfg.SweepInterval, err = durationEnv("GATEWAY_SWEEP_INTERVAL", 5*time.Minute)
	if err != nil {
		return nil, err
	}
	cfg.MaxIdleAge, err = durationEnv("GATEWAY_MAX_IDLE_AGE", 10*time.Minute)
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
		return fmt.Errorf("GATEWAY_LISTEN_ADDR is required")
	}
	if c.MetricsAddr == "" {
		return fmt.Errorf("GATEWAY_METRICS_ADDR is required")
	}

	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return fmt.Errorf("GATEWAY_TLS_CERT_FILE and GATEWAY_TLS_KEY_FILE must both be set or both empty")
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

	// Required unconditionally, not only for the player route table: this is the
	// player gateway, so there is no configuration in which it is optional.
	if c.PlayerJWKSURL == "" {
		return fmt.Errorf("GATEWAY_PLAYER_JWKS_URL is required")
	}
	if c.PlayerIssuer == "" {
		return fmt.Errorf("GATEWAY_PLAYER_ISSUER is required")
	}
	if c.PlayerAudience == "" {
		return fmt.Errorf("GATEWAY_PLAYER_AUDIENCE is required")
	}

	// Validated here, not at first fetch: a JWKS URL with a typo in it is
	// indistinguishable from Auth being down once the process is running, and
	// the two have very different fixes.
	for _, u := range []struct {
		key, val string
	}{
		{"GATEWAY_PLAYER_JWKS_URL", c.PlayerJWKSURL},
		{"GATEWAY_STAFF_JWKS_URL", c.StaffJWKSURL},
	} {
		if u.val == "" {
			continue
		}
		parsed, err := url.Parse(u.val)
		if err != nil {
			return fmt.Errorf("%s: %w", u.key, err)
		}
		if parsed.Scheme == "" || parsed.Host == "" {
			return fmt.Errorf("%s: must be an absolute URL, got %q", u.key, u.val)
		}
	}

	// A zero RPS with a positive burst admits the burst and then 429s every
	// request from that IP forever; a zero burst rejects everything. Neither is
	// a usable limit, so all four must be positive (techspec §6.4).
	for _, l := range []struct {
		key string
		val int
	}{
		{"GATEWAY_RATE_LIMIT_RPS", c.RateLimitRPS},
		{"GATEWAY_RATE_LIMIT_BURST", c.RateLimitBurst},
		{"GATEWAY_LOGIN_RATE_LIMIT_RPS", c.LoginRateLimitRPS},
		{"GATEWAY_LOGIN_RATE_LIMIT_BURST", c.LoginRateLimitBurst},
	} {
		if l.val <= 0 {
			return fmt.Errorf("%s must be positive, got %d", l.key, l.val)
		}
	}
	// A zero ticker interval panics in time.NewTicker; a zero idle age would
	// evict every entry on each sweep and reset every client's bucket.
	if c.SweepInterval <= 0 {
		return fmt.Errorf("GATEWAY_SWEEP_INTERVAL must be positive, got %v", c.SweepInterval)
	}
	if c.MaxIdleAge <= 0 {
		return fmt.Errorf("GATEWAY_MAX_IDLE_AGE must be positive, got %v", c.MaxIdleAge)
	}

	return nil
}

// parseTrustedProxies parses GATEWAY_TRUSTED_PROXIES: a comma-separated list of
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
				return nil, fmt.Errorf("GATEWAY_TRUSTED_PROXIES: invalid entry %q: %w", entry, err)
			}
			prefixes = append(prefixes, p.Masked())
			continue
		}
		addr, err := netip.ParseAddr(entry)
		if err != nil {
			return nil, fmt.Errorf("GATEWAY_TRUSTED_PROXIES: invalid entry %q: %w", entry, err)
		}
		addr = addr.Unmap()
		prefixes = append(prefixes, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return prefixes, nil
}

// parseUpstreams scans env for GATEWAY_UPSTREAM_<NAME>_URL vars.
// Names are normalised to lowercase: GATEWAY_UPSTREAM_PHPADMIN_URL → "phpadmin".
func parseUpstreams() (map[string]*url.URL, error) {
	upstreams := make(map[string]*url.URL)
	for _, env := range os.Environ() {
		k, v, ok := strings.Cut(env, "=")
		if !ok {
			continue
		}
		if !strings.HasPrefix(k, "GATEWAY_UPSTREAM_") || !strings.HasSuffix(k, "_URL") {
			continue
		}
		name := strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(k, "GATEWAY_UPSTREAM_"), "_URL"))
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
