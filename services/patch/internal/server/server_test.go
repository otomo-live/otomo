package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/patch/internal/config"
)

// harness is a running server plus the two addresses it bound, so a test can assert
// what each listener answers.
type harness struct {
	public   string
	internal string // the metrics listener: health, readiness, metrics, pprof
	api      string // the internal API listener (CF-3): server manifest and blobs
	client   *http.Client
}

func newHarness(t *testing.T, ready func(context.Context) error, manifests func() error) *harness {
	t.Helper()
	return newHarnessWithDeps(t, Deps{Ready: ready, Manifests: manifests})
}

// newHarnessWithDeps is newHarness with a caller-supplied Deps, so a manifest test can
// inject a Holder and a Verifier the way main does.
func newHarnessWithDeps(t *testing.T, deps Deps) *harness {
	t.Helper()

	cfg := config.Config{
		ListenAddr:      ":0",
		MetricsAddr:     ":0",
		InternalAddr:    ":0",
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
	srv.SetReady(true)

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
		public:   hostPort(t, srv.PublicAddr()),
		internal: hostPort(t, srv.MetricsAddr()),
		api:      hostPort(t, srv.InternalAddr()),
		client:   &http.Client{Timeout: 5 * time.Second},
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

func (h *harness) do(t *testing.T, base, path string) response {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+base+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
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

// TestPublicListenerAnswersCom5NotFound covers the only public route that exists
// today: every path is a COM-5 404. Asserting /healthz, /metrics and pprof keeps the
// two-listener split honest — none of the internal endpoints may be reachable on the
// public port, and an unknown path must not look different from a real one.
func TestPublicListenerAnswersCom5NotFound(t *testing.T) {
	h := newHarness(t, nil, nil)

	for _, path := range []string{
		"/",
		"/nope",
		"/patch/v1/live/manifest",
		"/healthz",
		"/readyz",
		"/metrics",
		"/debug/pprof/",
	} {
		t.Run(path, func(t *testing.T) {
			resp := h.do(t, h.public, path)
			if resp.status != http.StatusNotFound {
				t.Fatalf("GET %s = %d, want 404 (%s)", path, resp.status, resp.body)
			}
			if got := resp.code(t); got != "not_found" {
				t.Errorf("GET %s code = %q, want not_found", path, got)
			}
			if ct := resp.headers.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			if resp.headers.Get("X-Request-Id") == "" {
				t.Error("the response has no X-Request-Id")
			}
		})
	}
}

// TestInternalEndpointsStayOnTheInternalListener is the two-listener split: /healthz
// and /metrics exist, but not on the port the Gateway forwards to.
func TestInternalEndpointsStayOnTheInternalListener(t *testing.T) {
	h := newHarness(t, nil, nil)

	if resp := h.do(t, h.internal, "/healthz"); resp.status != http.StatusOK {
		t.Errorf("internal /healthz = %d, want 200", resp.status)
	}
	if resp := h.do(t, h.internal, "/readyz"); resp.status != http.StatusOK {
		t.Errorf("internal /readyz = %d, want 200 when every injected check passes (%s)", resp.status, resp.body)
	}
	if resp := h.do(t, h.internal, "/metrics"); !strings.Contains(resp.body, "patch_build_info") {
		t.Errorf("internal /metrics does not report patch_build_info:\n%s", resp.body)
	}
}

// TestReadyzConsultsTheInjectedChecks checks that the manifest check gates readiness
// on its own, and that a failing dependency is named in the body: a readiness probe
// that only says "not ready" is a probe nobody can act on.
func TestReadyzConsultsTheInjectedChecks(t *testing.T) {
	manifestsNotLoaded := func() error { return errors.New("manifests not loaded") }
	h := newHarness(t, nil, manifestsNotLoaded)

	resp := h.do(t, h.internal, "/readyz")
	if resp.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.status)
	}
	if !strings.Contains(resp.body, "manifests not loaded") {
		t.Errorf("body does not name the failing check: %s", resp.body)
	}

	// A database that is down is reported too, and before the manifest check, because
	// no manifest can be loaded without it.
	postgresDown := func(context.Context) error {
		return errors.New("postgres unreachable: dial tcp 10.0.0.1:5432: connect: connection refused")
	}
	unready := newHarness(t, postgresDown, nil)
	resp = unready.do(t, unready.internal, "/readyz")
	if resp.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.status)
	}
	if !strings.Contains(resp.body, "postgres unreachable") {
		t.Errorf("body does not name the failing dependency: %s", resp.body)
	}

	// The checks run per request, so a dependency that recovers must be able to make
	// the service ready without a restart.
	healthy := newHarness(t, func(context.Context) error { return nil }, func() error { return nil })
	if resp := healthy.do(t, healthy.internal, "/readyz"); resp.status != http.StatusOK {
		t.Errorf("status = %d, want 200 (%s)", resp.status, resp.body)
	}
}
