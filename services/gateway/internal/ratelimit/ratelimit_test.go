package ratelimit

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"
)

var ok = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
})

func request(h http.Handler, remoteAddr string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = remoteAddr
	h.ServeHTTP(rr, req)
	return rr
}

// Each client IP has its own bucket: one client spending its burst does not
// cost another client anything. The port is ignored, so a client cannot get a
// fresh bucket by opening a new connection.
func TestLimiter_PerIPBuckets(t *testing.T) {
	h := New(0, 1).Wrap(ok) // rate 0: the burst is all a client ever gets

	if rr := request(h, "10.0.0.1:1000"); rr.Code != http.StatusOK {
		t.Fatalf("first request from A: %d, want 200", rr.Code)
	}
	if rr := request(h, "10.0.0.1:2000"); rr.Code != http.StatusTooManyRequests {
		t.Errorf("A from a new port: %d, want 429 (same IP, same bucket)", rr.Code)
	}
	if rr := request(h, "10.0.0.2:1000"); rr.Code != http.StatusOK {
		t.Errorf("first request from B: %d, want 200 (own bucket)", rr.Code)
	}
}

// A 429 is written in the COM-5 shape (techspec §6.4 acceptance).
func TestLimiter_429IsCOM5(t *testing.T) {
	h := New(0, 1).Wrap(ok)
	request(h, "10.0.0.1:1")
	rr := request(h, "10.0.0.1:1")

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != "rate_limit_exceeded" {
		t.Errorf("error.code = %q, want rate_limit_exceeded", body.Error.Code)
	}
}

// The sweeper evicts entries idle longer than maxIdleAge (techspec §6.4), so
// the per-IP map cannot grow without bound. Fake time via synctest; rate 0
// makes eviction visible, because only a fresh entry has its burst back.
func TestSweeperEvictsIdleEntries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rl := New(0, 1)
		rl.StartSweeper(t.Context(), 5*time.Minute, 10*time.Minute)
		h := rl.Wrap(ok)

		if rr := request(h, "10.0.0.1:12345"); rr.Code != http.StatusOK {
			t.Fatalf("first request: %d, want 200 (burst 1)", rr.Code)
		}
		if rr := request(h, "10.0.0.1:12345"); rr.Code != http.StatusTooManyRequests {
			t.Fatalf("second request: %d, want 429 (rate 0, burst spent)", rr.Code)
		}

		// Past one sweep interval plus the idle age: the entry must be gone.
		time.Sleep(16 * time.Minute)
		synctest.Wait()

		if rr := request(h, "10.0.0.1:12345"); rr.Code != http.StatusOK {
			t.Errorf("after sweep: %d, want 200 (entry evicted, fresh burst)", rr.Code)
		}
	})
}
