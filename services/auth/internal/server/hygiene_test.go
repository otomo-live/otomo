package server_test

import (
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestLogsNeverCarryDeviceIDsOrTokens is AU-6's log-hygiene criterion against a real
// server and Postgres: a login with a known device ID, a second login, a refresh, a
// replayed refresh token (the warn-level reuse line), a logout and a refused body all
// happen, and then every log line is searched for the device ID and every token
// issued. None may appear. It also checks auth_logins_total and auth_refresh_total
// counted each step.
func TestLogsNeverCarryDeviceIDsOrTokens(t *testing.T) {
	e := newE2E(t)

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	device := base64.RawURLEncoding.EncodeToString(raw)
	login := `{"device_id":"` + device + `"}`

	first, _ := e.ok(t, "/auth/anonymous", login)
	second, _ := e.ok(t, "/auth/anonymous", login)
	rotated, _ := e.ok(t, "/auth/refresh", refreshBody(second.RefreshToken))
	e.expect(t, "/auth/refresh", refreshBody(second.RefreshToken), http.StatusUnauthorized, "invalid_token") // reuse
	e.expect(t, "/auth/logout", refreshBody(first.RefreshToken), http.StatusNoContent, "")
	e.expect(t, "/auth/refresh", refreshBody(first.RefreshToken), http.StatusUnauthorized, "invalid_token") // revoked
	// A body that is not the contract's still carries the device ID; it must not be
	// echoed into a log either.
	// The decoder ignores unknown fields, so the refusal comes from the device_id itself:
	// the device ID plus a character the contract doesn't allow.
	e.expect(t, "/auth/anonymous", `{"device_id":"`+device+`!"}`, http.StatusBadRequest, "validation_failed")

	logs := e.h.logs.String()
	if !strings.Contains(logs, "refresh_token_reuse") {
		t.Fatalf("the reuse was not logged, so this test is not looking at the right lines:\n%s", logs)
	}
	secrets := map[string]string{
		"device_id":                   device,
		"first access token":          first.AccessToken,
		"first refresh token":         first.RefreshToken,
		"second access token":         second.AccessToken,
		"second refresh token":        second.RefreshToken,
		"rotated access token":        rotated.AccessToken,
		"rotated refresh token":       rotated.RefreshToken,
		"a token's signature segment": first.AccessToken[strings.LastIndexByte(first.AccessToken, '.')+1:],
	}
	for name, secret := range secrets {
		if strings.Contains(logs, secret) {
			t.Errorf("the logs contain the %s", name)
		}
	}

	resp, err := http.Get(e.h.metrics + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{
		`auth_logins_total{new_account="true",result="ok"} 1`,
		`auth_logins_total{new_account="false",result="ok"} 1`,
		`auth_logins_total{new_account="false",result="invalid"} 1`,
		`auth_refresh_total{result="ok"} 1`,
		`auth_refresh_total{result="reuse_detected"} 1`,
		`auth_refresh_total{result="revoked"} 1`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/metrics has no %s", want)
		}
	}
	if strings.Contains(string(body), device) {
		t.Error("/metrics contains the device ID")
	}
}
