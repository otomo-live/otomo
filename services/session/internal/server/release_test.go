package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/otomo-live/otomo/services/session/internal/config"
)

// fakeHead is a channel head that moves to next when refreshed.
type fakeHead struct {
	head      atomic.Int64
	next      int64
	refreshes atomic.Int32
}

func (f *fakeHead) Head() int64 { return f.head.Load() }
func (f *fakeHead) Refresh(context.Context) int64 {
	f.refreshes.Add(1)
	if f.next != 0 {
		f.head.Store(f.next)
	}
	return f.head.Load()
}

func headAt(n int64) *fakeHead {
	f := &fakeHead{}
	f.head.Store(n)
	return f
}

func checked(t *testing.T, require bool, head ReleaseHead, release string) (int, string) {
	t.Helper()
	s := New(config.Config{RequireReleaseHeader: require},
		Deps{Release: head, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	h := s.releaseCheck(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/player/session/me", nil)
	if release != "" {
		req.Header.Set("X-Otomo-Release", release)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	code := ""
	if rec.Code >= 400 {
		code = errorCode(t, rec.Body.String())
	}
	return rec.Code, code
}

// TestOnlyTheHeadPasses is SE-8's criterion: any release but the head, older or newer
// (after a rollback), gets 409 release_outdated; the head passes.
func TestOnlyTheHeadPasses(t *testing.T) {
	for _, tt := range []struct {
		release string
		want    int
		code    string
	}{
		{"42", http.StatusNoContent, ""},
		{"41", http.StatusConflict, "release_outdated"},
		{"43", http.StatusConflict, "release_outdated"},
		{"abc", http.StatusBadRequest, "invalid_release"},
		{"0", http.StatusBadRequest, "invalid_release"},
		{"-3", http.StatusBadRequest, "invalid_release"},
	} {
		if got, code := checked(t, false, headAt(42), tt.release); got != tt.want || code != tt.code {
			t.Errorf("release %s = %d %s, want %d %s", tt.release, got, code, tt.want, tt.code)
		}
	}
}

// TestAMismatchRefreshesTheHeadFirst: a client that patched to a release published since
// the last poll passes once the head is refreshed.
func TestAMismatchRefreshesTheHeadFirst(t *testing.T) {
	head := headAt(42)
	head.next = 43
	if got, _ := checked(t, false, head, "43"); got != http.StatusNoContent {
		t.Errorf("the new release = %d, want 204 after a refresh", got)
	}
	if head.refreshes.Load() != 1 {
		t.Errorf("%d refreshes, want 1", head.refreshes.Load())
	}
	// The head needs no refresh.
	if got, _ := checked(t, false, head, "43"); got != http.StatusNoContent || head.refreshes.Load() != 1 {
		t.Errorf("the head = %d with %d refreshes, want 204 and no new refresh", got, head.refreshes.Load())
	}
}

// TestAMissingHeaderIsOneSwitch: let through until SESSION_REQUIRE_RELEASE_HEADER is
// set, then refused.
func TestAMissingHeaderIsOneSwitch(t *testing.T) {
	if got, _ := checked(t, false, headAt(42), ""); got != http.StatusNoContent {
		t.Errorf("no header, not required = %d, want 204", got)
	}
	if got, code := checked(t, true, headAt(42), ""); got != http.StatusConflict || code != "release_outdated" {
		t.Errorf("no header, required = %d %s, want 409 release_outdated", got, code)
	}
}

// TestAnUnknownHeadChecksNothing: before Patch has answered, or with the loader off,
// Patch being down must not take every player route with it.
func TestAnUnknownHeadChecksNothing(t *testing.T) {
	if got, _ := checked(t, false, headAt(0), "41"); got != http.StatusNoContent {
		t.Errorf("unknown head = %d, want 204", got)
	}
	if got, _ := checked(t, false, nil, "41"); got != http.StatusNoContent {
		t.Errorf("no head source = %d, want 204", got)
	}
}

// TestTheCheckIsOnPlayerRoutesOnly: through the real server, a player route answers 401
// before 409 and 409 for an outdated release; a staff route ignores the header.
func TestTheCheckIsOnPlayerRoutesOnly(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.Release = headAt(42) })
	send := func(path, token, release string) (int, string) {
		req, err := http.NewRequest(http.MethodGet, h.publicURL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("X-Otomo-Release", release)
		resp, err := h.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, errorCode(t, string(b))
	}

	if code, c := send("/api/player/session/me", "", "41"); code != http.StatusUnauthorized {
		t.Errorf("no token, old release = %d %s, want 401 first", code, c)
	}
	if code, c := send("/api/player/session/me", h.playerToken(t), "41"); code != http.StatusConflict || c != "release_outdated" {
		t.Errorf("player route, old release = %d %s, want 409 release_outdated", code, c)
	}
	if code, c := send("/api/player/session/events", h.playerToken(t), "41"); code != http.StatusConflict || c != "release_outdated" {
		t.Errorf("long-poll, old release = %d %s, want 409 release_outdated", code, c)
	}
	if code, _ := send("/api/player/session/me", h.playerToken(t), "42"); code != http.StatusNotImplemented {
		t.Errorf("player route, the head = %d, want it to reach the handler (501 here)", code)
	}
	if code, _ := send("/api/admin/session/audit", h.staffToken(t, "viewer"), "41"); code != http.StatusNotImplemented {
		t.Errorf("staff route, old release = %d, want it exempt (501 here)", code)
	}
	if got := testutil.ToFloat64(h.metrics.releaseChecks.WithLabelValues(releaseOutdated)); got != 2 {
		t.Errorf("session_release_checks_total{result=outdated} = %v, want 2", got)
	}
}
