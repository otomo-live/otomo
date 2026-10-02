package directory

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// testLogger keeps the directory's warnings out of the test output.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestRefreshKeepsLastGoodOnFailure pins the recovery rule: a poll that fails or
// answers a non-2xx leaves the previous snapshot in place, and the bearer key is
// actually sent.
func TestRefreshKeepsLastGoodOnFailure(t *testing.T) {
	var fail atomic.Bool
	var hits atomic.Int32
	body := []byte(`{"servers":[{"server_id":"gs-1","internal_addr":"127.0.0.1:1","state":"busy"}]}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if fail.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	d := New(Config{URL: srv.URL, Key: "test-key", Refresh: time.Hour, Client: srv.Client(), Logger: testLogger()})
	if err := d.Refresh(t.Context()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if s, ok := d.Lookup("gs-1"); !ok || s.InternalAddr != "127.0.0.1:1" {
		t.Fatalf("Lookup(gs-1) = %+v, %v; want the published server", s, ok)
	}

	fail.Store(true)
	if err := d.Refresh(t.Context()); err == nil {
		t.Fatal("Refresh succeeded while the Allocator was failing")
	}
	if _, ok := d.Lookup("gs-1"); !ok {
		t.Fatal("a failed refresh dropped the last good table")
	}
	if !d.Loaded() {
		t.Fatal("Loaded went false after a failed refresh")
	}
}

// TestRefreshIfStaleRateLimits covers the once-per-interval rule the proxy relies on
// when a ticket names an unknown server.
func TestRefreshIfStaleRateLimits(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"servers":[]}`))
	}))
	t.Cleanup(srv.Close)

	now := time.Now()
	d := New(Config{
		URL:     srv.URL,
		Key:     "test-key",
		Refresh: time.Hour,
		Client:  srv.Client(),
		Now:     func() time.Time { return now },
		Logger:  testLogger(),
	})
	if err := d.Refresh(t.Context()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	if d.RefreshIfStale(t.Context(), time.Second) {
		t.Fatal("RefreshIfStale refreshed inside the interval")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("HTTP hits = %d, want 1", got)
	}

	now = now.Add(2 * time.Second)
	if !d.RefreshIfStale(t.Context(), time.Second) {
		t.Fatal("RefreshIfStale did not refresh after the interval")
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("HTTP hits = %d, want 2", got)
	}
}
