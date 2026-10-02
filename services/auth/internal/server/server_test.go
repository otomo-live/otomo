package server_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/otomo-live/otomo/services/auth/internal/config"
	"github.com/otomo-live/otomo/services/auth/internal/server"
	"github.com/otomo-live/otomo/services/auth/internal/store"
	"github.com/otomo-live/otomo/services/auth/internal/token"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type fakeJWKS struct{ raw []byte }

func (f fakeJWKS) Bytes() []byte { return f.raw }

type panickingJWKS struct{}

func (panickingJWKS) Bytes() []byte { panic("jwks cache is empty") }

type blockingJWKS struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	raw     []byte
}

func (b *blockingJWKS) Bytes() []byte {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return b.raw
}

type harness struct {
	server  *server.Server
	public  string
	metrics string
	logs    *syncBuffer

	cancel   context.CancelFunc
	done     chan error
	stopOnce sync.Once
	runErr   error
}

func newHarness(t *testing.T, deps server.Deps) *harness {
	t.Helper()

	logs := &syncBuffer{}
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	}
	if deps.JWKS == nil {
		deps.JWKS = fakeJWKS{raw: []byte(`{"keys":[]}`)}
	}

	cfg := serverTestConfig()
	srv := server.New(cfg, deps)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	select {
	case <-srv.Started():
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("the server never started listening")
	}

	h := &harness{
		server:  srv,
		public:  "http://" + srv.PublicAddr().String(),
		metrics: "http://" + srv.MetricsAddr().String(),
		logs:    logs,
		cancel:  cancel,
		done:    done,
	}
	t.Cleanup(func() { h.stop(t) })
	return h
}

func serverTestConfig() config.Config {
	return config.Config{
		ListenAddr:      "127.0.0.1:0",
		MetricsAddr:     "127.0.0.1:0",
		ReadTimeout:     10 * time.Second,
		WriteTimeout:    30 * time.Second,
		IdleTimeout:     120 * time.Second,
		ShutdownTimeout: 10 * time.Second,
		AccessTokenTTL:  15 * time.Minute,
		RefreshTokenTTL: 720 * time.Hour,
	}
}

func (h *harness) stop(t *testing.T) {
	t.Helper()
	h.stopOnce.Do(func() {
		h.cancel()
		select {
		case h.runErr = <-h.done:
		case <-time.After(10 * time.Second):
			h.runErr = errors.New("Run never returned")
		}
		if h.runErr != nil {
			t.Errorf("Run returned %v, want nil after a clean shutdown", h.runErr)
		}
	})
}

func (h *harness) requestLines(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(h.logs.String()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		if entry["msg"] == "http_request" {
			out = append(out, entry)
		}
	}
	return out
}

func get(t *testing.T, url string) (int, http.Header, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, resp.Header, string(body)
}

func TestRequestIDIsGeneratedWhenAbsentAndEchoedWhenPresent(t *testing.T) {
	h := newHarness(t, server.Deps{})

	_, header, _ := get(t, h.public+"/.well-known/jwks.json")
	generated := header.Get("X-Request-Id")
	if generated == "" {
		t.Fatal("no X-Request-Id on the response")
	}
	if _, err := uuid.Parse(generated); err != nil {
		t.Errorf("generated X-Request-Id %q is not a UUID: %v", generated, err)
	}

	req, err := http.NewRequest(http.MethodGet, h.public+"/.well-known/jwks.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Request-Id", "gateway-supplied-id")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET with X-Request-Id: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Request-Id"); got != "gateway-supplied-id" {
		t.Errorf("X-Request-Id = %q, want the inbound value", got)
	}
}

func TestAccessLogUsesTheRoutePatternNotTheRawPath(t *testing.T) {
	h := newHarness(t, server.Deps{})

	resp, err := http.Post(h.public+"/auth/anonymous", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /auth/anonymous: %v", err)
	}
	resp.Body.Close()

	secret := "please-do-not-log-me"
	if status, _, _ := get(t, h.public+"/no/such/path?q="+secret); status != http.StatusNotFound {
		t.Errorf("unknown route status = %d, want 404", status)
	}

	var stub, unmatched bool
	for _, entry := range h.requestLines(t) {
		switch entry["route"] {
		case "/auth/anonymous":
			stub = true
			if status, _ := entry["status"].(float64); status != 501 {
				t.Errorf("stub status = %v, want 501", entry["status"])
			}
		case "/":
			unmatched = true
		}
	}
	if !stub {
		t.Errorf("no access-log line with route /auth/anonymous; log was:\n%s", h.logs.String())
	}
	if !unmatched {
		t.Errorf("no access-log line with route / for the fallback; log was:\n%s", h.logs.String())
	}
	if strings.Contains(h.logs.String(), secret) {
		t.Error("the access log contains the raw query string")
	}
}

func TestPanicIsRecoveredIntoA500ThatIsStillLoggedAndCounted(t *testing.T) {
	h := newHarness(t, server.Deps{JWKS: panickingJWKS{}})

	status, header, body := get(t, h.public+"/.well-known/jwks.json")
	if status != http.StatusInternalServerError {
		t.Fatalf("GET jwks with a panicking provider = %d, want 500", status)
	}

	var envelope struct {
		Error struct {
			Code      string `json:"code"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("500 body %q is not the COM-5 shape: %v", body, err)
	}
	if envelope.Error.Code != "internal_error" {
		t.Errorf("500 code = %q, want internal_error", envelope.Error.Code)
	}
	requestID := envelope.Error.RequestID
	if requestID == "" {
		t.Fatal("the 500 body has no request_id: the panic unwound past the request-ID context")
	}
	if got := header.Get("X-Request-Id"); got != requestID {
		t.Errorf("X-Request-Id = %q but the body request_id = %q; want the same", got, requestID)
	}

	var logged bool
	for _, entry := range h.requestLines(t) {
		if entry["route"] != "/.well-known/jwks.json" {
			continue
		}
		logged = true
		if status, _ := entry["status"].(float64); status != 500 {
			t.Errorf("access-log status = %v, want 500", entry["status"])
		}
		if id, _ := entry["request_id"].(string); id != requestID {
			t.Errorf("access-log request_id = %v, want %q", entry["request_id"], requestID)
		}
	}
	if !logged {
		t.Errorf("the panicking request produced no access-log line; log was:\n%s", h.logs.String())
	}

	_, _, metricsBody := get(t, h.metrics+"/metrics")
	want := `auth_http_requests_total{method="GET",route="/.well-known/jwks.json",status="500"} 1`
	if !strings.Contains(metricsBody, want) {
		t.Errorf("/metrics does not contain %q", want)
	}
}

func TestInternalEndpointsAreNotOnThePublicListener(t *testing.T) {
	h := newHarness(t, server.Deps{})

	for _, path := range []string{"/metrics", "/debug/pprof/", "/debug/pprof/goroutine", "/healthz", "/readyz"} {
		status, _, body := get(t, h.public+path)
		if status != http.StatusNotFound {
			t.Errorf("GET %s on the public listener = %d, want 404", path, status)
		}
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(body), &envelope); err != nil || envelope.Error.Code != "not_found" {
			t.Errorf("GET %s body = %q, want the COM-5 not_found shape", path, body)
		}
	}

	if status, _, _ := get(t, h.metrics+"/metrics"); status != http.StatusOK {
		t.Errorf("GET /metrics on the internal listener = %d, want 200", status)
	}
	if status, _, _ := get(t, h.metrics+"/debug/pprof/"); status != http.StatusOK {
		t.Errorf("GET /debug/pprof/ on the internal listener = %d, want 200", status)
	}
}

func TestMetricsExposeBuildInfoAndRequests(t *testing.T) {
	h := newHarness(t, server.Deps{Version: "test-sha", JWKSKeyCount: 2})

	if status, _, _ := get(t, h.public+"/.well-known/jwks.json"); status != http.StatusOK {
		t.Fatalf("GET jwks = %d, want 200", status)
	}

	_, _, body := get(t, h.metrics+"/metrics")
	for _, want := range []string{
		`auth_build_info{version="test-sha"} 1`,
		"auth_jwks_keys_active 2",
		`auth_http_requests_total{method="GET",route="/.well-known/jwks.json",status="200"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics does not contain %q", want)
		}
	}
}

func TestReadyzTracksTheReadyFlag(t *testing.T) {
	h := newHarness(t, server.Deps{Ready: func(context.Context) error { return nil }})

	if status, _, _ := get(t, h.metrics+"/healthz"); status != http.StatusOK {
		t.Errorf("/healthz = %d, want 200 throughout startup", status)
	}
	if status, _, _ := get(t, h.metrics+"/readyz"); status != http.StatusServiceUnavailable {
		t.Errorf("/readyz before startup completes = %d, want 503", status)
	}

	h.server.SetReady(true)

	if status, _, _ := get(t, h.metrics+"/readyz"); status != http.StatusOK {
		t.Errorf("/readyz after startup completes = %d, want 200", status)
	}
	if status, _, _ := get(t, h.metrics+"/healthz"); status != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", status)
	}
}

func TestReadyzFailsWhenTheDatabaseIsUnreachable(t *testing.T) {
	h := newHarness(t, server.Deps{
		Ready: func(context.Context) error { return errors.New("postgres unreachable") },
	})
	h.server.SetReady(true)

	status, _, body := get(t, h.metrics+"/readyz")
	if status != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d, want 503 while the database is down", status)
	}
	if !strings.Contains(body, `"not_ready"`) {
		t.Errorf("/readyz body = %q, want the COM-5 not_ready shape", body)
	}
}

func TestShutdownWaitsForAnInFlightRequest(t *testing.T) {
	blocking := &blockingJWKS{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		raw:     []byte(`{"keys":[]}`),
	}
	h := newHarness(t, server.Deps{JWKS: blocking})

	statuses := make(chan int, 1)
	failures := make(chan error, 1)
	go func() {
		resp, err := http.Get(h.public + "/.well-known/jwks.json")
		if err != nil {
			failures <- err
			return
		}
		defer resp.Body.Close()
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			failures <- err
			return
		}
		statuses <- resp.StatusCode
	}()

	select {
	case <-blocking.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the request never reached the handler")
	}

	h.cancel()
	time.Sleep(100 * time.Millisecond)

	close(blocking.release)

	select {
	case err := <-failures:
		t.Fatalf("the in-flight request was cut off by shutdown: %v", err)
	case status := <-statuses:
		if status != http.StatusOK {
			t.Errorf("in-flight request status = %d, want 200", status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the in-flight request never finished")
	}

	h.stop(t)
}

type stubAccounts struct{ id string }

func (s stubAccounts) FindOrCreateAccount(context.Context, string, string) (string, bool, error) {
	return s.id, false, nil
}

// stubRefresh accepts every insert and knows no token, so a refresh is always a 401
// and a logout always a 204.
type stubRefresh struct{}

func (stubRefresh) InsertRefreshToken(context.Context, string, []byte, time.Time) (string, error) {
	return uuid.New().String(), nil
}

func (stubRefresh) RotateRefreshToken(context.Context, store.RotateInput) (store.RotateResult, error) {
	return store.RotateResult{Outcome: store.RotateInvalid}, nil
}

func (stubRefresh) RevokeRefreshFamily(context.Context, []byte) error { return nil }

func post(t *testing.T, url, body string) (int, http.Header, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, resp.Header, string(raw)
}

func TestAuthRoutesAreServedOnlyWhenTheStoresAndSignerAreWired(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wired := newHarness(t, server.Deps{
		Accounts: stubAccounts{id: uuid.New().String()},
		Refresh:  stubRefresh{},
		Signer: &token.Signer{
			Kid: "test", PrivateKey: priv,
			Issuer: "https://auth.otomo.internal", Audience: "otomo:player",
			TTL: 15 * time.Minute,
		},
	})
	device := strings.Repeat("d", 43)

	status, _, body := post(t, wired.public+"/auth/anonymous", `{"device_id":"`+device+`"}`)
	if status != http.StatusOK || !strings.Contains(body, `"refresh_token"`) {
		t.Errorf("wired POST /auth/anonymous = %d %s, want 200 with tokens", status, body)
	}
	// No services hand-off: the login body carries tokens only (D2).
	if strings.Contains(body, `"services"`) {
		t.Errorf("wired POST /auth/anonymous body %s still carries a services object", body)
	}

	unknown := `{"refresh_token":"` + strings.Repeat("A", 43) + `"}`
	status, header, body := post(t, wired.public+"/auth/refresh", unknown)
	if status != http.StatusUnauthorized || !strings.Contains(body, `"invalid_token"`) {
		t.Errorf("wired POST /auth/refresh = %d %s, want 401 invalid_token", status, body)
	}
	if got := header.Get("WWW-Authenticate"); got != "" {
		t.Errorf("POST /auth/refresh sent WWW-Authenticate %q", got)
	}
	if status, _, body := post(t, wired.public+"/auth/logout", unknown); status != http.StatusNoContent {
		t.Errorf("wired POST /auth/logout = %d %s, want 204", status, body)
	}

	for _, path := range []string{"/auth/anonymous", "/auth/refresh", "/auth/logout"} {
		status, header, body := get(t, wired.public+path)
		if status != http.StatusMethodNotAllowed || header.Get("Allow") != http.MethodPost {
			t.Errorf("GET %s = %d, Allow %q; want 405 with Allow: POST (%s)",
				path, status, header.Get("Allow"), body)
		}
	}

	// The access log names the route pattern and never the body, which carries the
	// device_id.
	for _, line := range wired.requestLines(t) {
		raw, _ := json.Marshal(line)
		if strings.Contains(string(raw), device) {
			t.Errorf("the device_id reached the access log: %s", raw)
		}
	}

	bare := newHarness(t, server.Deps{})
	for _, path := range []string{"/auth/anonymous", "/auth/refresh", "/auth/logout"} {
		if status, _, body := post(t, bare.public+path, `{}`); status != http.StatusNotImplemented {
			t.Errorf("unwired POST %s = %d %s, want 501", path, status, body)
		}
	}
}
