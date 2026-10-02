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

	"github.com/otomo-live/otomo/services/admin_auth/internal/config"
	"github.com/otomo-live/otomo/services/admin_auth/internal/server"
	"github.com/otomo-live/otomo/services/admin_auth/internal/token"
)

type fakeJWKS struct{ raw []byte }

func (f fakeJWKS) Bytes() []byte { return f.raw }

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

type harness struct {
	server  *server.Server
	public  string
	metrics string
	logs    *syncBuffer

	cancel context.CancelFunc
	done   chan error
	runErr error
}

func newHarness(t *testing.T, deps server.Deps) *harness {
	t.Helper()
	return newHarnessWithConfig(t, serverTestConfig(), deps)
}

// newHarnessWithConfig is newHarness with a caller-supplied config, so a test that
// needs non-zero token lifetimes or login settings does not have to mutate shared
// state.
func newHarnessWithConfig(t *testing.T, cfg config.Config, deps server.Deps) *harness {
	t.Helper()

	logs := &syncBuffer{}
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	}
	if deps.Keys == nil {
		deps.Keys = func() error { return nil }
	}
	if deps.JWKS == nil {
		deps.JWKS = fakeJWKS{raw: []byte(`{"keys":[]}`)}
	}

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
	}
}

func (h *harness) stop(t *testing.T) {
	t.Helper()
	h.cancel()
	select {
	case h.runErr = <-h.done:
	case <-time.After(10 * time.Second):
		h.runErr = errors.New("Run never returned")
	}
	if h.runErr != nil {
		t.Errorf("Run returned %v, want nil after a clean shutdown", h.runErr)
	}
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

func errorCode(t *testing.T, body string) string {
	t.Helper()
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("body %q is not the COM-5 shape: %v", body, err)
	}
	return envelope.Error.Code
}

func TestRequestIDIsGeneratedWhenAbsentAndEchoedWhenPresent(t *testing.T) {
	h := newHarness(t, server.Deps{})

	_, header, _ := get(t, h.public+"/no/such/path")
	generated := header.Get("X-Request-Id")
	if generated == "" {
		t.Fatal("no X-Request-Id on the response")
	}
	if _, err := uuid.Parse(generated); err != nil {
		t.Errorf("generated X-Request-Id %q is not a UUID: %v", generated, err)
	}

	req, err := http.NewRequest(http.MethodGet, h.public+"/no/such/path", nil)
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

func TestUnknownPathIsCOM5NotFound(t *testing.T) {
	h := newHarness(t, server.Deps{})

	status, _, body := get(t, h.public+"/anything")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
	if code := errorCode(t, body); code != "not_found" {
		t.Errorf("code = %q, want not_found", code)
	}
	if !strings.Contains(body, "no such route") {
		t.Errorf("body = %q, want the no-such-route message", body)
	}
}

func TestInternalEndpointsAreNotOnThePublicListener(t *testing.T) {
	h := newHarness(t, server.Deps{})

	for _, path := range []string{"/metrics", "/healthz", "/readyz", "/debug/pprof/"} {
		status, _, body := get(t, h.public+path)
		if status != http.StatusNotFound {
			t.Errorf("GET %s on the public listener = %d, want 404", path, status)
		}
		if code := errorCode(t, body); code != "not_found" {
			t.Errorf("GET %s code = %q, want not_found", path, code)
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
		`admin_auth_build_info{version="test-sha"} 1`,
		"admin_auth_jwks_keys_active 2",
		`admin_auth_http_requests_total{method="GET",route="/.well-known/jwks.json",status="200"} 1`,
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

func TestJWKSRouteServesThePublishedKey(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	raw, err := token.BuildJWKS([]token.PublicKey{{Kid: "admin-auth-test", Key: pub}})
	if err != nil {
		t.Fatalf("BuildJWKS: %v", err)
	}

	h := newHarness(t, server.Deps{
		JWKS:         fakeJWKS{raw: raw},
		JWKSKeyCount: 1,
		Ready:        func(context.Context) error { return nil },
	})
	h.server.SetReady(true)

	status, header, body := get(t, h.public+"/.well-known/jwks.json")
	if status != http.StatusOK {
		t.Fatalf("GET jwks = %d, want 200", status)
	}
	if got := header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}

	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Crv string `json:"crv"`
			Kid string `json:"kid"`
			X   string `json:"x"`
			Use string `json:"use"`
			Alg string `json:"alg"`
		} `json:"keys"`
	}
	if err := json.Unmarshal([]byte(body), &set); err != nil {
		t.Fatalf("JWKS body %q is not JSON: %v", body, err)
	}
	if len(set.Keys) != 1 {
		t.Fatalf("len(keys) = %d, want 1", len(set.Keys))
	}
	key := set.Keys[0]
	if key.Kty != "OKP" || key.Crv != "Ed25519" {
		t.Errorf("key = %+v, want an OKP/Ed25519 key", key)
	}
	if key.Kid != "admin-auth-test" {
		t.Errorf("kid = %q, want admin-auth-test", key.Kid)
	}
	if key.Use != "sig" || key.Alg != "EdDSA" {
		t.Errorf("use/alg = %q/%q, want sig/EdDSA", key.Use, key.Alg)
	}

	if status, _, _ := get(t, h.metrics+"/readyz"); status != http.StatusOK {
		t.Errorf("/readyz = %d, want 200 once a key is loaded", status)
	}
}

func TestReadyzFailsWhenNoSigningKeysAreLoaded(t *testing.T) {
	h := newHarness(t, server.Deps{
		Ready: func(context.Context) error { return nil },
		Keys:  func() error { return errors.New("no signing keys loaded") },
	})
	h.server.SetReady(true)

	status, _, body := get(t, h.metrics+"/readyz")
	if status != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d, want 503 while the JWKS holds no key", status)
	}
	if code := errorCode(t, body); code != "not_ready" {
		t.Errorf("/readyz code = %q, want not_ready", code)
	}
	if !strings.Contains(body, "no signing keys loaded") {
		t.Errorf("/readyz body = %q, want the signing-key message", body)
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
	if code := errorCode(t, body); code != "not_ready" {
		t.Errorf("/readyz code = %q, want not_ready", code)
	}
	if !strings.Contains(body, "postgres unreachable") {
		t.Errorf("/readyz body = %q, want the database message", body)
	}
}
