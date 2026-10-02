package api_test

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/otomo-live/otomo/services/auth/internal/api"
)

// recordedOutcomes is an api.Outcomes that keeps every count.
type recordedOutcomes struct {
	mu      sync.Mutex
	logins  []string // result + "/" + new_account
	refresh []string
}

func (r *recordedOutcomes) Login(result string, newAccount bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := "false"
	if newAccount {
		n = "true"
	}
	r.logins = append(r.logins, result+"/"+n)
}

func (r *recordedOutcomes) Refresh(result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refresh = append(r.refresh, result)
}

// TestEveryLoginAndRefreshIsCounted is AU-6's metrics criterion at the handler: each
// outcome, including reuse_detected, is counted once with its own result.
func TestEveryLoginAndRefreshIsCounted(t *testing.T) {
	f := newSessionFixture(t)
	device := newDeviceID(t)

	first := f.post("/auth/anonymous", `{"device_id":"`+device+`"}`)
	again := f.post("/auth/anonymous", `{"device_id":"`+device+`"}`)
	f.post("/auth/anonymous", `{"device_id":"short"}`)
	if first.Code != http.StatusOK || again.Code != http.StatusOK {
		t.Fatalf("logins = %d, %d", first.Code, again.Code)
	}
	tok := decodeLogin(t, again).RefreshToken

	f.refreshWith(tok)                                // ok
	f.refreshWith(tok)                                // reuse_detected
	f.refreshWith("not-a-token")                      // invalid
	f.post("/auth/refresh", `{"refresh":1}`)          // invalid body
	f.refreshWith(decodeLogin(t, first).RefreshToken) // ok (a different family)

	wantLogins := []string{"ok/true", "ok/false", "invalid/false"}
	wantRefresh := []string{"ok", "reuse_detected", "invalid", "invalid", "ok"}
	if strings.Join(f.outcomes.logins, ",") != strings.Join(wantLogins, ",") {
		t.Errorf("logins counted %v, want %v", f.outcomes.logins, wantLogins)
	}
	if strings.Join(f.outcomes.refresh, ",") != strings.Join(wantRefresh, ",") {
		t.Errorf("refreshes counted %v, want %v", f.outcomes.refresh, wantRefresh)
	}
}

// TestAFailedLoginIsCountedAndLogsNoSecret: the internal-error path logs the cause, and
// neither that line nor any other carries the device ID.
func TestAFailedLoginIsCountedAndLogsNoSecret(t *testing.T) {
	f := newSessionFixture(t)
	f.accounts.err = errors.New("postgres: connection refused")
	device := newDeviceID(t)

	if w := f.post("/auth/anonymous", `{"device_id":"`+device+`"}`); w.Code != http.StatusInternalServerError {
		t.Fatalf("login with the store down = %d, want 500", w.Code)
	}
	if strings.Join(f.outcomes.logins, ",") != "error/false" {
		t.Errorf("counted %v, want error/false", f.outcomes.logins)
	}
	logs := f.logs.String()
	if !strings.Contains(logs, "internal error") {
		t.Fatalf("no internal error line, so this test is not looking at the right lines:\n%s", logs)
	}
	if strings.Contains(logs, device) {
		t.Error("the logs contain the device ID")
	}
}

// TestHandlerLogsCarryNoSecrets: every secret a whole session creates, searched for in
// every line the handlers logged, reuse included.
func TestHandlerLogsCarryNoSecrets(t *testing.T) {
	f := newSessionFixture(t)
	device := newDeviceID(t)
	w := f.post("/auth/anonymous", `{"device_id":"`+device+`"}`)
	login := decodeLogin(t, w)
	next := decodeLogin(t, f.refreshWith(login.RefreshToken))
	f.refreshWith(login.RefreshToken) // reuse: the one warn line
	f.post("/auth/logout", `{"refresh_token":"`+next.RefreshToken+`"}`)

	logs := f.logs.String()
	if !strings.Contains(logs, "refresh_token_reuse") {
		t.Fatalf("the reuse was not logged:\n%s", logs)
	}
	for name, secret := range map[string]string{
		"device_id": device, "access token": login.AccessToken, "refresh token": login.RefreshToken,
		"second access token": next.AccessToken, "second refresh token": next.RefreshToken,
	} {
		if strings.Contains(logs, secret) {
			t.Errorf("the logs contain the %s", name)
		}
	}
}

var _ api.Outcomes = (*recordedOutcomes)(nil)
