package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/gameplay_proxy/internal/config"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startServer binds the private listener on a random loopback port and returns it.
func startServer(t *testing.T, ready func(ctx context.Context) error) (*Server, string) {
	t.Helper()
	s := New(config.Config{MetricsAddr: "127.0.0.1:0", ShutdownTimeout: 5 * time.Second}, Deps{
		Version: "test",
		Logger:  discardLogger(),
		Ready:   ready,
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Run(ctx) }()
	select {
	case <-s.Started():
	case <-time.After(2 * time.Second):
		t.Fatal("server did not start")
	}
	t.Cleanup(cancel)
	return s, "http://" + s.Addr().String()
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, string(body)
}

// TestHealthzReadyzAndMetrics covers the private listener's contract: liveness is
// always up, readiness follows the injected check, and /metrics serves the registry.
// The ready flag is atomic because the handler runs on the server's goroutine.
func TestHealthzReadyzAndMetrics(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)

	s, base := startServer(t, func(context.Context) error {
		if ready.Load() {
			return nil
		}
		return errors.New("not ready")
	})
	s.SetReady(true)

	if code, body := get(t, base+"/healthz"); code != http.StatusOK || body != "ok" {
		t.Errorf("healthz = %d %q, want 200 ok", code, body)
	}
	if code, _ := get(t, base+"/readyz"); code != http.StatusOK {
		t.Errorf("readyz while ready = %d, want 200", code)
	}

	ready.Store(false)
	if code, body := get(t, base+"/readyz"); code != http.StatusServiceUnavailable || !strings.Contains(body, "not ready") {
		t.Errorf("readyz while unready = %d %q, want 503", code, body)
	}

	if code, body := get(t, base+"/metrics"); code != http.StatusOK || !strings.Contains(body, "gameplay_proxy_build_info") {
		t.Errorf("metrics = %d, body contains build info: %v", code, strings.Contains(body, "gameplay_proxy_build_info"))
	}
}

// TestReadyzWaitsForSetReady checks the start-up gate: /readyz stays 503 until the
// caller says the wiring is complete, even if the injected check would pass.
func TestReadyzWaitsForSetReady(t *testing.T) {
	_, base := startServer(t, nil)
	if code, _ := get(t, base+"/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("readyz before SetReady = %d, want 503", code)
	}
}
