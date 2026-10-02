package server_test

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// metricsBody scrapes the internal listener's Prometheus exposition.
func metricsBody(t *testing.T, h *harness) string {
	t.Helper()

	status, _, body := get(t, h.metrics+"/metrics")
	if status != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", status)
	}
	return body
}

// counterValue extracts one labelled counter sample. It fails the test rather than
// returning zero when the series is absent, so a rename cannot silently pass a
// zero assertion.
func counterValue(t *testing.T, body, metric, result string) float64 {
	t.Helper()

	prefix := fmt.Sprintf(`%s{result=%q}`, metric, result)
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(line)
		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		return v
	}
	t.Fatalf("metric %s is missing from /metrics", prefix)
	return 0
}

func TestLoginMetricsAreCountedByOutcome(t *testing.T) {
	env := newAdminEnv(t)
	_, email, _ := env.staff(t, "viewer")
	pw := "password-" + email

	// A good login, a wrong password and a malformed body are the three outcomes.
	if status, _, body := postLogin(t, env.h.public, fmt.Sprintf(`{"email":%q,"password":%q}`, email, pw)); status != http.StatusOK {
		t.Fatalf("good login status = %d, want 200 (body %s)", status, body)
	}
	if status, _, body := postLogin(t, env.h.public, fmt.Sprintf(`{"email":%q,"password":"wrong"}`, email)); status != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d, want 401 (body %s)", status, body)
	}
	if status, _, body := postLogin(t, env.h.public, `{`); status != http.StatusBadRequest {
		t.Fatalf("malformed body status = %d, want 400 (body %s)", status, body)
	}

	body := metricsBody(t, env.h)
	for result, want := range map[string]float64{
		"success":             1,
		"invalid_credentials": 1,
		"bad_request":         1,
		"locked":              0,
		"error":               0,
	} {
		if got := counterValue(t, body, "staff_login_total", result); got != want {
			t.Errorf("staff_login_total{result=%q} = %v, want %v", result, got, want)
		}
	}
}

func TestRefreshMetricsAreCountedByOutcome(t *testing.T) {
	env := newSessionEnv(t, 30*time.Second)
	const pw = "refresh-metrics-password"
	email := uniqueLoginEmail("RefreshMetrics")
	insertStaff(t, env.db, email, "Refresh Metrics", pw, []string{"viewer"})

	cookie, _ := loginCookie(t, env.h, email, pw)

	// A real rotation is a success.
	if status, _, body := postRefresh(t, env.h.public, cookie); status != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200 (body %s)", status, body)
	}

	// Age just this row's rotation, then replay the old cookie: reuse.
	oldHash := sha256.Sum256([]byte(cookie.Value))
	if _, err := env.db.Pool.Exec(t.Context(),
		`UPDATE refresh_session SET rotated_at = now() - interval '1 minute' WHERE token_hash = $1`, oldHash[:]); err != nil {
		t.Fatalf("age the rotation: %v", err)
	}
	if status, _, body := postRefresh(t, env.h.public, cookie); status != http.StatusUnauthorized {
		t.Fatalf("replay status = %d, want 401 (body %s)", status, body)
	}

	// No cookie at all is an invalid presentation.
	if status, _, _ := postRefresh(t, env.h.public, nil); status != http.StatusUnauthorized {
		t.Fatalf("no-cookie refresh status = %d, want 401", status)
	}

	body := metricsBody(t, env.h)
	for result, want := range map[string]float64{
		"success":        1,
		"reuse_detected": 1,
		"invalid":        1,
		"error":          0,
	} {
		if got := counterValue(t, body, "staff_refresh_total", result); got != want {
			t.Errorf("staff_refresh_total{result=%q} = %v, want %v", result, got, want)
		}
	}
}
