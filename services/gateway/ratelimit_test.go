package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"gateway/gateway/internal/obslog"
	"gateway/gateway/internal/router"
)

// GATE-3 rate limiting (techspec §6.4), end to end through buildPublicMux.
// §6.4's acceptance: "Exceeding GATEWAY_RATE_LIMIT_RPS on /auth/* -> 429 in
// the COM-5 shape". The limiter itself (per-IP buckets, sweeper) is unit
// tested in internal/ratelimit.

// Every login pattern must be a real route, or a rename would silently drop
// the stricter limit from the brute-force target.
func TestLoginPatterns_ExistInRouteTable(t *testing.T) {
	patterns := make(map[string]bool)
	for _, r := range router.BuildRoutes() {
		patterns[r.Pattern] = true
	}
	for p := range loginPatterns {
		if !patterns[p] {
			t.Errorf("login pattern %q is not a route in router.BuildRoutes()", p)
		}
	}
}

// With a login limit of 1/1, a second login request from the same IP is a
// 429 in the COM-5 shape and never reaches the upstream, while a non-login
// public route from that IP is still served by the general limiter.
func TestRateLimit_LoginRouteUsesStricterLimiter(t *testing.T) {
	gw := buildTestGW(t, map[string]string{
		"GATEWAY_LOGIN_RATE_LIMIT_RPS":   "1",
		"GATEWAY_LOGIN_RATE_LIMIT_BURST": "1",
	})

	if status, _, proxied := gw.doProxied(t, http.MethodPost, "/auth/login", ""); status != http.StatusOK || !proxied {
		t.Fatalf("first login request: status=%d proxied=%v, want 200 proxied", status, proxied)
	}
	if status, code, proxied := gw.doProxied(t, http.MethodPost, "/auth/login", ""); status != http.StatusTooManyRequests || code != "rate_limit_exceeded" || proxied {
		t.Errorf("second login request: status=%d code=%q proxied=%v, want 429 rate_limit_exceeded, not proxied", status, code, proxied)
	}
	for i := 0; i < 3; i++ {
		if status, _, _ := gw.doProxied(t, http.MethodGet, "/patch/v1/live/manifest", ""); status != http.StatusOK {
			t.Errorf("non-login public request %d: status=%d, want 200 (general limiter)", i, status)
		}
	}
}

// The general limiter covers every other route, protected ones included, and
// runs before authentication: a client over its limit gets 429 without a
// signature check, and a valid token does not buy extra requests.
func TestRateLimit_GeneralLimiterAppliesBeforeAuth(t *testing.T) {
	gw := buildTestGW(t, map[string]string{
		"GATEWAY_RATE_LIMIT_RPS":   "1",
		"GATEWAY_RATE_LIMIT_BURST": "1",
	})

	if status, _, _ := gw.doProxied(t, http.MethodGet, "/api/player/session/x", gw.playerTok); status != http.StatusOK {
		t.Fatalf("first request: status=%d, want 200", status)
	}
	for _, tok := range []string{gw.playerTok, "", "not-a-jwt"} {
		if status, code, proxied := gw.doProxied(t, http.MethodGet, "/api/player/session/x", tok); status != http.StatusTooManyRequests || code != "rate_limit_exceeded" || proxied {
			t.Errorf("token %q over the limit: status=%d code=%q proxied=%v, want 429 before auth", tok, status, code, proxied)
		}
	}
}

// A 429 is not a token rejection: the access log line carries the route's
// group and an empty reason, and gateway_token_rejected_total does not move.
func TestRateLimit_429IsNotATokenRejection(t *testing.T) {
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, nil)))
	t.Cleanup(obslog.Init)

	gw := buildTestGW(t, map[string]string{
		"GATEWAY_RATE_LIMIT_RPS":   "1",
		"GATEWAY_RATE_LIMIT_BURST": "1",
	})
	const path = "/api/player/session/x"
	gw.do(t, http.MethodGet, path, "").Body.Close() // spends the burst (401 missing_token)

	reasons := []string{"missing_token", "invalid_signature", "expired", "aud_mismatch", "iss_mismatch", "insufficient_role", "invalid_token"}
	before := map[string]float64{}
	for _, r := range reasons {
		before[r] = testutil.ToFloat64(obslog.TokenRejected.WithLabelValues(r, "player"))
	}

	resp := gw.do(t, http.MethodGet, path, "")
	defer resp.Body.Close()
	var body struct {
		Error struct {
			Code      string `json:"code"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode COM-5 body: %v", err)
	}
	if resp.StatusCode != http.StatusTooManyRequests || body.Error.Code != "rate_limit_exceeded" {
		t.Fatalf("status=%d code=%q, want 429 rate_limit_exceeded", resp.StatusCode, body.Error.Code)
	}

	for _, r := range reasons {
		if after := testutil.ToFloat64(obslog.TokenRejected.WithLabelValues(r, "player")); after != before[r] {
			t.Errorf("gateway_token_rejected_total{reason=%q,group=\"player\"} moved on a 429: %v -> %v", r, before[r], after)
		}
	}

	rid := resp.Header.Get("X-Request-Id")
	var line string
	for _, l := range strings.Split(logBuf.String(), "\n") {
		if strings.Contains(l, fmt.Sprintf("%q:%q", "request_id", rid)) && strings.Contains(l, `"msg":"request"`) {
			line = l
		}
	}
	for _, want := range []string{`"status":429`, `"group":"player"`, `"reason":""`} {
		if !strings.Contains(line, want) {
			t.Errorf("access log line for the 429 lacks %s\nline: %s", want, line)
		}
	}
}
