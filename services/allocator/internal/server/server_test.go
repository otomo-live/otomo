package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/allocator/internal/config"
)

// harness is a running server plus the two addresses it bound, so a test can assert
// what each listener answers. It does not call SetReady: readiness is the subject of
// some tests, so each one sets the flag it needs.
type harness struct {
	srv     *Server
	api     string
	metrics string
	client  *http.Client
}

func newHarness(t *testing.T, deps Deps) *harness {
	t.Helper()

	cfg := config.Config{
		ListenAddr:      ":0",
		MetricsAddr:     ":0",
		ReadTimeout:     5 * time.Second,
		WriteTimeout:    5 * time.Second,
		IdleTimeout:     30 * time.Second,
		ShutdownTimeout: 5 * time.Second,
		LogLevel:        slog.LevelError,
	}
	deps.Version = "test"
	// A failing request should fail on its assertion, not on log noise.
	deps.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(cfg, deps)

	ctx, cancel := context.WithCancel(t.Context())
	runErr := make(chan error, 1)
	go func() { runErr <- srv.Run(ctx) }()

	select {
	case <-srv.Started():
	case <-time.After(10 * time.Second):
		t.Fatal("the server never bound its listeners")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-runErr:
			if err != nil {
				t.Errorf("Run returned %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Run did not return after the context was cancelled")
		}
	})

	return &harness{
		srv:     srv,
		api:     hostPort(t, srv.APIAddr()),
		metrics: hostPort(t, srv.MetricsAddr()),
		client:  &http.Client{Timeout: 5 * time.Second},
	}
}

// hostPort converts a listener's address into something a client can dial. A ":0"
// listener reports an unspecified host, which is not dialable on every platform.
func hostPort(t *testing.T, addr net.Addr) string {
	t.Helper()
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		t.Fatalf("cannot read a port out of %q: %v", addr, err)
	}
	return "127.0.0.1:" + port
}

type response struct {
	status  int
	headers http.Header
	body    string
}

func (h *harness) get(t *testing.T, base, path string, header http.Header) response {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+base+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for key, values := range header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return response{status: resp.StatusCode, headers: resp.Header, body: string(body)}
}

// code pulls the COM-5 error code out of a response and checks the two fields every
// error envelope must carry.
func (r response) code(t *testing.T) string {
	t.Helper()

	var env struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(r.body), &env); err != nil {
		t.Fatalf("body is not the COM-5 envelope: %v (%s)", err, r.body)
	}
	if env.Error.Message == "" {
		t.Error("the error envelope has no message")
	}
	if env.Error.RequestID == "" {
		t.Error("the error envelope has no request_id")
	}
	return env.Error.Code
}

// TestHealthzAnswersRegardlessOfReadiness keeps liveness dumb: /healthz must not
// consult the readiness flag or the database, or a dependency blip would get the
// container restarted.
func TestHealthzAnswersRegardlessOfReadiness(t *testing.T) {
	h := newHarness(t, Deps{})

	resp := h.get(t, h.metrics, "/healthz", nil)
	if resp.status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.status, resp.body)
	}
	if resp.body != "ok" {
		t.Errorf("body = %q, want ok", resp.body)
	}
	if ct := resp.headers.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", ct)
	}
}

// TestReadyzGatesOnSetReadyAndDepsReady checks the three transitions an operator
// cares about: starting up, ready, and a named dependency failing.
func TestReadyzGatesOnSetReadyAndDepsReady(t *testing.T) {
	h := newHarness(t, Deps{Ready: func(context.Context) error { return nil }})

	// Before the caller has finished start-up.
	resp := h.get(t, h.metrics, "/readyz", nil)
	if resp.status != http.StatusServiceUnavailable {
		t.Fatalf("readyz before SetReady = %d, want 503 (%s)", resp.status, resp.body)
	}
	if !strings.Contains(resp.body, "starting up") {
		t.Errorf("body does not say why: %s", resp.body)
	}

	h.srv.SetReady(true)
	resp = h.get(t, h.metrics, "/readyz", nil)
	if resp.status != http.StatusOK {
		t.Fatalf("readyz after SetReady = %d, want 200 (%s)", resp.status, resp.body)
	}
	if resp.body != "ok" {
		t.Errorf("body = %q, want ok", resp.body)
	}
	if ct := resp.headers.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", ct)
	}

	// And the reverse: ready as a flag, but the dependency says no.
	h.srv.SetReady(false)
	down := newHarness(t, Deps{Ready: func(context.Context) error {
		return errors.New("postgres unreachable: connection refused")
	}})
	down.srv.SetReady(true)
	resp = down.get(t, down.metrics, "/readyz", nil)
	if resp.status != http.StatusServiceUnavailable {
		t.Fatalf("readyz with a failing dependency = %d, want 503 (%s)", resp.status, resp.body)
	}
	if !strings.Contains(resp.body, "postgres unreachable") {
		t.Errorf("body does not name the failing dependency: %s", resp.body)
	}
	// A failed readiness check is the one health answer that is not plain text: it
	// keeps the COM-5 envelope so the reason travels in the standard error shape.
	if ct := resp.headers.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if !strings.Contains(resp.body, `"code":"not_ready"`) {
		t.Errorf("body = %q, want code not_ready", resp.body)
	}
}

// TestUnknownRouteIsCom5NotFound covers the only API route that exists today: every
// path is a COM-5 404. Requesting the internal endpoints keeps the two-listener split
// honest — none of them may be reachable on the API port.
func TestUnknownRouteIsCom5NotFound(t *testing.T) {
	h := newHarness(t, Deps{})

	for _, path := range []string{
		"/",
		"/nope",
		"/allocator/v1/servers",
		"/healthz",
		"/readyz",
		"/metrics",
		"/debug/pprof/",
	} {
		t.Run(path, func(t *testing.T) {
			resp := h.get(t, h.api, path, nil)
			if resp.status != http.StatusNotFound {
				t.Fatalf("GET %s = %d, want 404 (%s)", path, resp.status, resp.body)
			}
			if got := resp.code(t); got != "not_found" {
				t.Errorf("GET %s code = %q, want not_found", path, got)
			}
			if ct := resp.headers.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
		})
	}
}

// TestRequestIDIsEchoedValidatedAndGenerated checks the three cases the middleware
// has: a usable inbound ID is reused, a missing one is generated, and a broken one is
// replaced rather than trusted.
func TestRequestIDIsEchoedValidatedAndGenerated(t *testing.T) {
	h := newHarness(t, Deps{})

	hdr := http.Header{"X-Request-Id": []string{"gateway-abc-123"}}
	resp := h.get(t, h.api, "/nope", hdr)
	if got := resp.headers.Get("X-Request-Id"); got != "gateway-abc-123" {
		t.Errorf("X-Request-Id = %q, want the inbound value", got)
	}
	if got := resp.code(t); got != "not_found" {
		t.Errorf("code = %q, want not_found", got)
	}

	resp = h.get(t, h.api, "/nope", nil)
	generated := resp.headers.Get("X-Request-Id")
	if _, err := hex.DecodeString(generated); err != nil || len(generated) != 32 {
		t.Errorf("generated X-Request-Id = %q, want 32 hex characters", generated)
	}

	// Over 128 bytes is not a usable ID, so it must be replaced rather than echoed
	// into every log line for the request.
	long := strings.Repeat("a", 200)
	resp = h.get(t, h.api, "/nope", http.Header{"X-Request-Id": []string{long}})
	if got := resp.headers.Get("X-Request-Id"); got == long {
		t.Error("the server echoed an over-long X-Request-Id")
	} else if len(got) != 32 {
		t.Errorf("X-Request-Id = %q, want a generated 32-hex ID", got)
	}
}

// TestRoutesAreReachable confirms the seam later tickets use: a handler registered
// through Deps.Routes is served, and the access path still returns the request ID.
func TestRoutesAreReachable(t *testing.T) {
	h := newHarness(t, Deps{Routes: func(mux *http.ServeMux) {
		mux.HandleFunc("GET /allocator/v1/ping", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("pong"))
		})
	}})

	resp := h.get(t, h.api, "/allocator/v1/ping", nil)
	if resp.status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.status, resp.body)
	}
	if resp.body != "pong" {
		t.Errorf("body = %q, want pong", resp.body)
	}
	if resp.headers.Get("X-Request-Id") == "" {
		t.Error("the response has no X-Request-Id")
	}
}

// TestPanicIsRecoveredAndTheServerKeepsServing checks that a panicking handler yields a
// COM-5 500 and, crucially, that the process and both listeners survive it.
func TestPanicIsRecoveredAndTheServerKeepsServing(t *testing.T) {
	h := newHarness(t, Deps{Routes: func(mux *http.ServeMux) {
		mux.HandleFunc("GET /boom", func(w http.ResponseWriter, r *http.Request) {
			panic("boom")
		})
	}})

	resp := h.get(t, h.api, "/boom", nil)
	if resp.status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", resp.status, resp.body)
	}
	if got := resp.code(t); got != "internal_error" {
		t.Errorf("code = %q, want internal_error", got)
	}
	// The panic message must not reach the client.
	if strings.Contains(resp.body, "boom") {
		t.Errorf("the response leaked the panic value: %s", resp.body)
	}

	if resp := h.get(t, h.metrics, "/healthz", nil); resp.status != http.StatusOK {
		t.Errorf("healthz after a panic = %d, want 200", resp.status)
	}
	if resp := h.get(t, h.api, "/nope", nil); resp.status != http.StatusNotFound {
		t.Errorf("unknown route after a panic = %d, want 404", resp.status)
	}
}

// TestMetricsReportBuildInfoAndBoundedRoutes checks the registry the later tickets
// build on: it is exposed, it carries build info, and an unknown path collapses to the
// "unmatched" route label instead of a series per path.
func TestMetricsReportBuildInfoAndBoundedRoutes(t *testing.T) {
	h := newHarness(t, Deps{})
	if h.srv.Registry() == nil {
		t.Fatal("Registry() returned nil")
	}

	h.get(t, h.api, "/some/unknown/path", nil)

	body := h.get(t, h.metrics, "/metrics", nil).body
	for _, want := range []string{
		"allocator_build_info",
		`allocator_http_requests_total{method="GET",route="unmatched",status="404"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics does not contain %q:\n%s", want, body)
		}
	}
}
