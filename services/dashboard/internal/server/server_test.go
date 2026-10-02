package server

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/otomo-live/otomo/services/dashboard/internal/api"
	"github.com/otomo-live/otomo/services/dashboard/internal/auditsrc"
	"github.com/otomo-live/otomo/services/dashboard/internal/auth"
	"github.com/otomo-live/otomo/services/dashboard/internal/config"
	"github.com/otomo-live/otomo/services/dashboard/internal/health"
	"github.com/otomo-live/otomo/services/dashboard/internal/promql"
	"github.com/otomo-live/otomo/services/dashboard/internal/source"
)

const (
	testKid      = "test-staff-key"
	testIssuer   = "https://php-admin.otomo.internal"
	testAudience = "otomo:staff"
)

// jwksDoc renders a one-key JWKS document for pub.
func jwksDoc(t *testing.T, pub ed25519.PublicKey) []byte {
	t.Helper()
	doc, err := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "OKP",
		"crv": "Ed25519",
		"kid": testKid,
		"x":   base64.RawURLEncoding.EncodeToString(pub),
		"alg": "EdDSA",
		"use": "sig",
	}}})
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}
	return doc
}

// harness is a running server plus the key its tokens must be signed with, so a test can
// assert both halves of the contract: what the listener answers, and that the answer came
// from verifying a real token rather than from a stub.
type harness struct {
	public   string
	internal string
	priv     ed25519.PrivateKey
	client   *http.Client
	srv      *Server
}

func newHarness(t *testing.T, ready func(context.Context) error) *harness {
	t.Helper()
	return newHarnessWithHandlers(t, ready, nil)
}

func newHarnessWithHandlers(t *testing.T, ready func(context.Context) error, handlers *api.Handlers) *harness {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	doc := jwksDoc(t, pub)
	jwksSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(doc)
	}))
	t.Cleanup(jwksSrv.Close)

	verifier := auth.NewVerifier(jwksSrv.URL, testIssuer, testAudience, time.Minute, 0)
	verifier.Start(t.Context())

	cfg := config.Config{
		ListenAddr:      ":0",
		MetricsAddr:     ":0",
		ReadTimeout:     5 * time.Second,
		WriteTimeout:    5 * time.Second,
		IdleTimeout:     30 * time.Second,
		ShutdownTimeout: 5 * time.Second,
		LogLevel:        slog.LevelError,
	}
	srv := New(cfg, Deps{
		Verifier: verifier,
		Handlers: handlers,
		Ready:    ready,
		Version:  "test",
		// A failing request should fail on its assertion, not on log noise.
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
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

	// The wait for the first key fetch is what makes the rejection tests meaningful:
	// otherwise a 401 could mean "the verifier has no keys yet".
	deadline := time.Now().Add(5 * time.Second)
	for !verifier.Ready(t.Context()) {
		if time.Now().After(deadline) {
			t.Fatal("the verifier never fetched the jwks")
		}
		time.Sleep(5 * time.Millisecond)
	}

	return &harness{
		public:   hostPort(t, srv.PublicAddr()),
		internal: hostPort(t, srv.MetricsAddr()),
		priv:     priv,
		client:   &http.Client{Timeout: 5 * time.Second},
		srv:      srv,
	}
}

// hostPort converts a listener's address into something a client can dial. A ":0" listener
// reports an unspecified host, which is not dialable on every platform.
func hostPort(t *testing.T, addr net.Addr) string {
	t.Helper()
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		t.Fatalf("cannot read a port out of %q: %v", addr, err)
	}
	return "127.0.0.1:" + port
}

// token mints a staff token, with mut available to corrupt one claim.
func (h *harness) token(t *testing.T, roles []string, mut func(jwt.MapClaims)) string {
	t.Helper()

	now := time.Now()
	claims := jwt.MapClaims{
		"iss":   testIssuer,
		"aud":   testAudience,
		"sub":   "staff-1",
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
		"roles": roles,
	}
	if mut != nil {
		mut(claims)
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["kid"] = testKid
	signed, err := tok.SignedString(h.priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

type response struct {
	status  int
	headers http.Header
	body    string
}

func (h *harness) do(t *testing.T, base, method, path, token string) response {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+base+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return response{status: resp.StatusCode, headers: resp.Header, body: string(body)}
}

// code pulls the COM-5 error code out of a response.
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

// routedPaths expands the route table into concrete request paths, filling the one wildcard
// so a test can walk every route without knowing which ones carry {name}.
func routedPaths() []string {
	paths := make([]string, 0, 6)
	for _, rt := range api.Routes() {
		paths = append(paths, strings.ReplaceAll(rt.Path, "{name}", "gateway"))
	}
	return paths
}

// TestPublicListenerAdmitsOnlyStaffTokens is DSH-C1/DSH-C2's acceptance criterion, from the
// outside: a missing token and a player token must both be 401, and each must name its own
// reason rather than sharing a generic one.
func TestPublicListenerAdmitsOnlyStaffTokens(t *testing.T) {
	h := newHarness(t, nil)

	// A token from the player domain: correctly signed, wrong service. This is the case
	// that a shared key between domains would otherwise let through, so it is asserted
	// against a real signature rather than a garbage string.
	playerToken := h.token(t, []string{"viewer"}, func(c jwt.MapClaims) {
		c["iss"] = "https://auth.otomo.internal"
		c["aud"] = "otomo:player"
	})

	// A token signed by a key this service has never seen.
	_, otherKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	forged := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{
		"iss": testIssuer,
		"aud": testAudience,
		"sub": "staff-1",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	forged.Header["kid"] = testKid
	forgedToken, err := forged.SignedString(otherKey)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	tests := []struct {
		name   string
		token  string
		reason string
	}{
		{"no token", "", auth.ReasonMissingToken},
		{"not a token", "garbage", auth.ReasonInvalidToken},
		{"player domain", playerToken, auth.ReasonIssuerMismatch},
		{"forged signature", forgedToken, auth.ReasonInvalidSignature},
	}

	// Every route, plus an unknown path, so the boundary is asserted on the whole surface
	// rather than on a sample of it.
	paths := append(routedPaths(), "/api/admin/dashboard/nope")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, path := range paths {
				resp := h.do(t, h.public, http.MethodGet, path, tt.token)

				if resp.status != http.StatusUnauthorized {
					t.Errorf("GET %s = %d, want 401 (%s)", path, resp.status, resp.body)
					continue
				}
				if got := resp.code(t); got != tt.reason {
					t.Errorf("GET %s code = %q, want %q", path, got, tt.reason)
				}
				if resp.headers.Get("X-Request-Id") == "" {
					t.Errorf("GET %s has no X-Request-Id", path)
				}
			}
		})
	}
}

// TestViewerReachesEveryRoute walks the whole table with a viewer token. The 501 is the
// assertion that the request got past the role check and reached the stub: a wrong role
// would have produced a 403, and the two are easy to confuse if only the rejection case is
// tested. The series route is implemented now, so a viewer reaching it without its required
// metric gets the handler's own 400 instead.
func TestViewerReachesEveryRoute(t *testing.T) {
	// Non-nil Handlers so the routes that are implemented run their real handler; the rest
	// still land on the 501 placeholder.
	h := newHarnessWithHandlers(t, nil, &api.Handlers{})
	token := h.token(t, []string{"viewer"}, nil)

	implemented := map[string]struct {
		status int
		code   string
	}{
		"/api/admin/dashboard/overview":                {http.StatusOK, ""},
		"/api/admin/dashboard/services":                {http.StatusOK, ""},
		"/api/admin/dashboard/services/gateway/series": {http.StatusBadRequest, "unknown_metric"},
		// Logs and the tail are implemented now; with no LogSource wired they fail
		// closed with a 502 rather than reaching the 501 placeholder.
		"/api/admin/dashboard/logs":      {http.StatusBadGateway, "upstream_error"},
		"/api/admin/dashboard/logs/tail": {http.StatusBadGateway, "upstream_error"},
		// Audit is implemented too; with no Audit clients wired every feed is missing
		// and the handler fails closed with a 502.
		"/api/admin/dashboard/audit": {http.StatusBadGateway, "upstream_error"},
	}

	for _, path := range routedPaths() {
		resp := h.do(t, h.public, http.MethodGet, path, token)
		wantStatus, wantCode := http.StatusNotImplemented, "not_implemented"
		if impl, ok := implemented[path]; ok {
			wantStatus, wantCode = impl.status, impl.code
		}
		if resp.status != wantStatus {
			t.Errorf("GET %s = %d, want %d (%s)", path, resp.status, wantStatus, resp.body)
			continue
		}
		if wantCode == "" {
			continue
		}
		if got := resp.code(t); got != wantCode {
			t.Errorf("GET %s code = %q, want %q", path, got, wantCode)
		}
	}
}

// TestStaffWithoutRolesIsForbidden is the other half of the role boundary: a token that is
// correctly signed, correctly issued and carries no role this service knows must be
// refused with 403 — retrying it changes nothing — rather than admitted.
func TestStaffWithoutRolesIsForbidden(t *testing.T) {
	h := newHarness(t, nil)

	for _, roles := range [][]string{nil, {"supervisor"}} {
		token := h.token(t, roles, nil)
		for _, path := range routedPaths() {
			resp := h.do(t, h.public, http.MethodGet, path, token)
			if resp.status != http.StatusForbidden {
				t.Errorf("GET %s with roles %v = %d, want 403 (%s)", path, roles, resp.status, resp.body)
				continue
			}
			if got := resp.code(t); got != auth.ReasonInsufficientRole {
				t.Errorf("GET %s code = %q, want %q", path, got, auth.ReasonInsufficientRole)
			}
		}
	}
}

// TestRejectionsAreCountedByReason ties the response body to the metric: an operator
// alerting on the counter needs it to move for the same reason the client was told.
func TestRejectionsAreCountedByReason(t *testing.T) {
	h := newHarness(t, nil)

	before := counterValue(t, h, "dashboard_token_rejected_total", `reason="insufficient_role"`)
	if resp := h.do(t, h.public, http.MethodGet, "/api/admin/dashboard/overview", h.token(t, nil, nil)); resp.status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.status)
	}
	after := counterValue(t, h, "dashboard_token_rejected_total", `reason="insufficient_role"`)
	if after != before+1 {
		t.Errorf("dashboard_token_rejected_total{reason=insufficient_role} went %g -> %g, want exactly +1", before, after)
	}

	missing := counterValue(t, h, "dashboard_token_rejected_total", `reason="missing_token"`)
	if resp := h.do(t, h.public, http.MethodGet, "/api/admin/dashboard/overview", ""); resp.status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.status)
	}
	if got := counterValue(t, h, "dashboard_token_rejected_total", `reason="missing_token"`); got != missing+1 {
		t.Errorf("dashboard_token_rejected_total{reason=missing_token} went %g -> %g, want exactly +1", missing, got)
	}
}

// counterValue reads one series out of the Prometheus text exposition, and returns 0 when
// the series is absent — which is what an unlabelled-yet counter actually is, and the state
// of every reason series before the first rejection of that kind.
func counterValue(t *testing.T, h *harness, name, labels string) float64 {
	t.Helper()

	body := h.do(t, h.internal, http.MethodGet, "/metrics", "").body
	prefix := name + "{" + labels
	for _, line := range strings.Split(body, "\n") {
		// labels is a subset of the sample's label set, so it must match up to a comma or
		// the closing brace rather than anywhere in the line.
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		if rest := line[len(prefix):]; !strings.HasPrefix(rest, ",") && !strings.HasPrefix(rest, "}") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("cannot read a value out of %q", line)
		}
		var v float64
		if _, err := fmt.Sscanf(fields[1], "%g", &v); err != nil {
			t.Fatalf("cannot parse %q: %v", fields[1], err)
		}
		return v
	}
	return 0
}

// TestUnmatchedRequestsAreGuardedToo covers the fallback: an unauthenticated request for a
// path that does not exist must be answered like one for a path that does, so the service
// cannot be used to map its own route table from outside.
func TestUnmatchedRequestsAreGuardedToo(t *testing.T) {
	h := newHarness(t, nil)

	t.Run("without a token", func(t *testing.T) {
		resp := h.do(t, h.public, http.MethodGet, "/api/admin/dashboard/nope", "")
		if resp.status != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.status)
		}
		if got := resp.code(t); got != auth.ReasonMissingToken {
			t.Errorf("code = %q, want %q", got, auth.ReasonMissingToken)
		}
	})

	t.Run("with a viewer token", func(t *testing.T) {
		resp := h.do(t, h.public, http.MethodGet, "/api/admin/dashboard/nope", h.token(t, []string{"viewer"}, nil))
		if resp.status != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.status)
		}
		if got := resp.code(t); got != "not_found" {
			t.Errorf("code = %q, want not_found", got)
		}
	})

	t.Run("wrong method", func(t *testing.T) {
		resp := h.do(t, h.public, http.MethodDelete, "/api/admin/dashboard/overview", h.token(t, []string{"viewer"}, nil))
		if resp.status != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", resp.status)
		}
		if got := resp.headers.Get("Allow"); got != "GET, HEAD" {
			t.Errorf("Allow = %q, want \"GET, HEAD\"", got)
		}
	})
}

// TestInternalEndpointsStayOnTheInternalListener is the two-listener split: /metrics and
// /healthz exist, but not on the port Gateway forwards to. A viewer token must not find them
// there.
func TestInternalEndpointsStayOnTheInternalListener(t *testing.T) {
	h := newHarness(t, nil)
	token := h.token(t, []string{"viewer"}, nil)

	for _, path := range []string{"/healthz", "/readyz", "/metrics", "/debug/pprof/"} {
		resp := h.do(t, h.public, http.MethodGet, path, token)
		if resp.status != http.StatusNotFound {
			t.Errorf("public %s = %d, want 404", path, resp.status)
		}
	}

	if resp := h.do(t, h.internal, http.MethodGet, "/healthz", ""); resp.status != http.StatusOK {
		t.Errorf("internal /healthz = %d, want 200", resp.status)
	}
	if resp := h.do(t, h.internal, http.MethodGet, "/metrics", ""); !strings.Contains(resp.body, "dashboard_build_info") {
		t.Errorf("internal /metrics does not report dashboard_build_info:\n%s", resp.body)
	}
}

// TestReadyzIs503UntilTheJwksHasKeys pins the readiness rule from DSH-C2's side: the
// Dashboard is ready only once it can verify a token. The JWKS endpoint deliberately answers
// an empty key set first, so the verifier is running but has nothing to verify with yet.
//
// This test builds the server without running it, so it can observe the transition directly
// instead of racing a background refresh against a listener.
func TestReadyzIs503UntilTheJwksHasKeys(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	doc := jwksDoc(t, pub)

	var serving atomic.Bool
	jwksSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if serving.Load() {
			_, _ = w.Write(doc)
			return
		}
		_, _ = w.Write([]byte(`{"keys":[]}`))
	}))
	t.Cleanup(jwksSrv.Close)

	verifier := auth.NewVerifier(jwksSrv.URL, testIssuer, testAudience, 20*time.Millisecond, 0)
	verifier.Start(t.Context())

	srv := New(config.Config{ListenAddr: ":0", MetricsAddr: ":0"}, Deps{
		Verifier: verifier,
		Ready: func(ctx context.Context) error {
			if !verifier.Ready(ctx) {
				return errors.New("staff jwks not fetched yet from " + verifier.URL())
			}
			return nil
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	srv.SetReady(true)

	readyz := func() response {
		w := httptest.NewRecorder()
		srv.internalHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		return response{status: w.Code, headers: w.Header(), body: w.Body.String()}
	}

	if resp := readyz(); resp.status != http.StatusServiceUnavailable {
		t.Fatalf("readyz before the jwks had keys = %d, want 503", resp.status)
	} else if !strings.Contains(resp.body, "staff jwks not fetched") {
		t.Errorf("503 body does not name the reason: %s", resp.body)
	}

	serving.Store(true)

	deadline := time.Now().Add(5 * time.Second)
	for !verifier.Ready(t.Context()) {
		if time.Now().After(deadline) {
			t.Fatal("the verifier never fetched the jwks")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if resp := readyz(); resp.status != http.StatusOK {
		t.Fatalf("readyz after the jwks had keys = %d, want 200 (%s)", resp.status, resp.body)
	}
}

// TestPublicHandlerRefusesWithoutAVerifier pins the fail-closed direction for a server built
// with a zero-value Deps, which is what every other test relies on being safe.
func TestPublicHandlerRefusesWithoutAVerifier(t *testing.T) {
	srv := New(config.Config{ListenAddr: ":0", MetricsAddr: ":0"}, Deps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	w := httptest.NewRecorder()
	srv.publicHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/admin/dashboard/overview", nil))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

// waitProber waits for one target to reach a state, with a bounded deadline so a broken
// prober fails the test rather than hanging it.
func waitProber(t *testing.T, p *health.Prober, name string, ok func(health.Status) bool) health.Status {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for {
		for _, s := range p.Snapshot() {
			if s.Name == name && ok(s) {
				return s
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("prober target %q never reached the expected state", name)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestServicesEndpointThroughTheServer is DSH-C4 from the outside: a viewer token reaches
// the real handler, and the response is the prober's snapshot in the documented shape.
func TestServicesEndpointThroughTheServer(t *testing.T) {
	t.Run("two targets sorted by name", func(t *testing.T) {
		newTarget := func(body string) *httptest.Server {
			return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if body == "" {
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, "ok")
					return
				}
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, body)
			}))
		}
		authSrv := newTarget("")
		configSrv := newTarget("database unreachable")
		t.Cleanup(authSrv.Close)
		t.Cleanup(configSrv.Close)

		prober := health.NewProber([]health.Target{
			{Name: "patch", URL: authSrv.URL},
			{Name: "auth", URL: authSrv.URL},
			{Name: "config", URL: configSrv.URL},
		}, time.Hour, time.Second, nil)
		h := newHarnessWithHandlers(t, nil, &api.Handlers{Services: prober.Snapshot})
		prober.Observer = h.srv
		go prober.Run(t.Context())

		waitProber(t, prober, "auth", func(s health.Status) bool { return s.Ready })
		waitProber(t, prober, "config", func(s health.Status) bool { return !s.CheckedAt.IsZero() })

		resp := h.do(t, h.public, http.MethodGet, "/api/admin/dashboard/services", h.token(t, []string{"viewer"}, nil))
		if resp.status != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", resp.status, resp.body)
		}
		if ct := resp.headers.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}

		var body struct {
			Services []struct {
				Name        string     `json:"name"`
				Up          bool       `json:"up"`
				Ready       bool       `json:"ready"`
				Reason      string     `json:"reason"`
				LatencyMs   int64      `json:"latency_ms"`
				CheckedAt   time.Time  `json:"checked_at"`
				LastReadyAt *time.Time `json:"last_ready_at"`
			} `json:"services"`
		}
		if err := json.Unmarshal([]byte(resp.body), &body); err != nil {
			t.Fatalf("body: %v (%s)", err, resp.body)
		}
		if len(body.Services) != 3 {
			t.Fatalf("got %d services, want 3", len(body.Services))
		}
		wantOrder := []string{"auth", "config", "patch"}
		for i, want := range wantOrder {
			if body.Services[i].Name != want {
				t.Errorf("services[%d].Name = %q, want %q", i, body.Services[i].Name, want)
			}
		}
		auth := body.Services[0]
		if !auth.Up || !auth.Ready || auth.Reason != "" || auth.LatencyMs < 0 || auth.CheckedAt.IsZero() || auth.LastReadyAt == nil {
			t.Errorf("auth service = %+v", auth)
		}
		config := body.Services[1]
		if !config.Up || config.Ready || config.Reason != "database unreachable" {
			t.Errorf("config service = %+v", config)
		}
	})

	t.Run("no targets is an empty list", func(t *testing.T) {
		h := newHarnessWithHandlers(t, nil, &api.Handlers{})
		resp := h.do(t, h.public, http.MethodGet, "/api/admin/dashboard/services", h.token(t, []string{"viewer"}, nil))
		if resp.status != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", resp.status, resp.body)
		}
		if !strings.Contains(resp.body, `"services":[]`) {
			t.Errorf("body = %s, want an empty services array", resp.body)
		}
	})
}

// TestServiceProbeMetrics ties the prober's results to the internal listener: an operator
// reading /metrics must see the same up/ready the /services handler just reported.
func TestServiceProbeMetrics(t *testing.T) {
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	downSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "not ready")
	}))
	t.Cleanup(upSrv.Close)
	t.Cleanup(downSrv.Close)

	prober := health.NewProber([]health.Target{
		{Name: "auth", URL: upSrv.URL},
		{Name: "config", URL: downSrv.URL},
	}, time.Hour, time.Second, nil)
	h := newHarnessWithHandlers(t, nil, &api.Handlers{Services: prober.Snapshot})
	prober.Observer = h.srv
	go prober.Run(t.Context())

	waitProber(t, prober, "auth", func(s health.Status) bool { return s.Ready })
	waitProber(t, prober, "config", func(s health.Status) bool { return !s.CheckedAt.IsZero() && !s.Ready })

	body := h.do(t, h.internal, http.MethodGet, "/metrics", "").body
	for _, want := range []string{
		`dashboard_service_up{service="auth"} 1`,
		`dashboard_service_ready{service="auth"} 1`,
		`dashboard_service_up{service="config"} 1`,
		`dashboard_service_ready{service="config"} 0`,
		`dashboard_probe_duration_seconds_count{service="auth"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics does not contain %q", want)
		}
	}
}

// stubMetrics is an empty source.MetricsSource: every overview card degrades, which is
// still a 200. It keeps the endpoint test independent of Prometheus.
type stubMetrics struct{}

func (stubMetrics) Instant(context.Context, string) (map[string]float64, error) {
	return map[string]float64{}, nil
}

func (stubMetrics) Range(context.Context, string, time.Time, time.Time, time.Duration) ([]int64, map[string][]float64, error) {
	return nil, nil, nil
}

func (stubMetrics) Targets(context.Context) ([]source.Target, error) {
	return nil, nil
}

func (stubMetrics) Alerts(context.Context) ([]source.Alert, error) {
	return nil, nil
}

// TestOverviewEndpointThroughTheServer is DSH-C5 from the outside: a viewer token reaches
// the real handler and gets the documented JSON, an anonymous caller gets 401.
func TestOverviewEndpointThroughTheServer(t *testing.T) {
	h := newHarnessWithHandlers(t, nil, &api.Handlers{Metrics: stubMetrics{}})

	resp := h.do(t, h.public, http.MethodGet, "/api/admin/dashboard/overview", h.token(t, []string{"viewer"}, nil))
	if resp.status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.status, resp.body)
	}
	if ct := resp.headers.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if cc := resp.headers.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}

	var body struct {
		GeneratedAt string   `json:"generated_at"`
		Degraded    []string `json:"degraded"`
	}
	if err := json.Unmarshal([]byte(resp.body), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, resp.body)
	}
	if body.GeneratedAt == "" {
		t.Error("generated_at is empty")
	}
	if _, err := time.Parse(time.RFC3339, body.GeneratedAt); err != nil {
		t.Errorf("generated_at %q is not RFC3339: %v", body.GeneratedAt, err)
	}
	if body.Degraded == nil {
		t.Error("degraded is null, want a list")
	}

	anon := h.do(t, h.public, http.MethodGet, "/api/admin/dashboard/overview", "")
	if anon.status != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401 (%s)", anon.status, anon.body)
	}
}

// seriesSource is a minimal source.MetricsSource for the series endpoint test: one
// unlabelled line, so the handler names it after the template.
type seriesSource struct{}

func (seriesSource) Instant(context.Context, string) (map[string]float64, error) {
	return map[string]float64{}, nil
}

func (seriesSource) Range(_ context.Context, _ string, from, to time.Time, step time.Duration) ([]int64, map[string][]float64, error) {
	ts := []int64{from.Unix(), from.Unix() + int64(step/time.Second)}
	return ts, map[string][]float64{"{}": {1, math.NaN()}}, nil
}

func (seriesSource) Targets(context.Context) ([]source.Target, error) { return nil, nil }

func (seriesSource) Alerts(context.Context) ([]source.Alert, error) { return nil, nil }

// TestSeriesEndpointThroughTheServer is DSH-C6 from the outside: a viewer token reaches the
// real handler and gets the documented columnar JSON, while an anonymous caller gets 401.
func TestSeriesEndpointThroughTheServer(t *testing.T) {
	h := newHarnessWithHandlers(t, nil, &api.Handlers{
		Metrics: seriesSource{},
		AllowList: func(context.Context) (promql.ServiceSet, error) {
			return promql.NewServiceSet("gateway"), nil
		},
	})

	to := time.Now().Unix()
	from := to - 3600
	path := "/api/admin/dashboard/services/gateway/series?metric=latency_p95&from=" +
		strconv.FormatInt(from, 10) + "&to=" + strconv.FormatInt(to, 10)
	resp := h.do(t, h.public, http.MethodGet, path, h.token(t, []string{"viewer"}, nil))
	if resp.status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.status, resp.body)
	}
	if ct := resp.headers.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if cc := resp.headers.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}

	var body struct {
		Metric  string `json:"metric"`
		Service string `json:"service"`
		Step    int64  `json:"step"`
		T       []int64
		Series  []struct {
			Name   string     `json:"name"`
			Values []*float64 `json:"values"`
		} `json:"series"`
	}
	if err := json.Unmarshal([]byte(resp.body), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, resp.body)
	}
	if body.Metric != "latency_p95" || body.Service != "gateway" || body.Step != 15 {
		t.Errorf("metadata = %+v", body)
	}
	if len(body.T) != 2 || len(body.Series) != 1 || body.Series[0].Name != "latency_p95" {
		t.Fatalf("columnar body = %+v", body)
	}
	if body.Series[0].Values[1] != nil {
		t.Errorf("NaN value = %v, want null", body.Series[0].Values[1])
	}

	anon := h.do(t, h.public, http.MethodGet, path, "")
	if anon.status != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401 (%s)", anon.status, anon.body)
	}
}

// staffAuditFeed is a fake audit upstream for the end-to-end audit test.
type staffAuditFeed struct {
	entries []auditsrc.Entry
	mu      sync.Mutex
	gotAuth string
}

func (f *staffAuditFeed) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.gotAuth = r.Header.Get("Authorization")
	f.mu.Unlock()

	start := 0
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		start, _ = strconv.Atoi(raw)
	}
	limit := 200
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, _ = strconv.Atoi(raw)
	}
	if start > len(f.entries) {
		start = len(f.entries)
	}
	end := start + limit
	if end > len(f.entries) {
		end = len(f.entries)
	}

	body := map[string]any{"entries": f.entries[start:end], "next_cursor": nil}
	if end < len(f.entries) {
		body["next_cursor"] = strconv.Itoa(end)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func (f *staffAuditFeed) auth() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotAuth
}

// TestAuditEndpointThroughTheServer is DSH-C11 from the outside: a viewer token reaches the
// merged audit handler, the caller's token is forwarded to both feeds, and the walk returns
// both feeds' entries exactly once in global order.
func TestAuditEndpointThroughTheServer(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	config := []auditsrc.Entry{
		{ID: 3, At: base.Add(3 * time.Second), Source: "config"},
		{ID: 1, At: base.Add(1 * time.Second), Source: "config"},
	}
	admin := []auditsrc.Entry{
		{ID: 4, At: base.Add(2 * time.Second), Source: "admin-auth"},
		{ID: 2, At: base, Source: "admin-auth"},
	}
	// Session's feed (SE-7): a force-disband between the others.
	session := []auditsrc.Entry{
		{ID: 1, At: base.Add(1500 * time.Millisecond), Source: "session"},
	}
	cf := &staffAuditFeed{entries: config}
	af := &staffAuditFeed{entries: admin}
	sf := &staffAuditFeed{entries: session}
	csrv := httptest.NewServer(cf)
	defer csrv.Close()
	asrv := httptest.NewServer(af)
	defer asrv.Close()
	ssrv := httptest.NewServer(sf)
	defer ssrv.Close()

	h := newHarnessWithHandlers(t, nil, &api.Handlers{Audit: []*auditsrc.Client{
		auditsrc.New("config", csrv.URL, "/api/admin/config/audit", nil, time.Second),
		auditsrc.New("admin-auth", asrv.URL, "/admin-auth/audit", nil, time.Second),
		auditsrc.New("session", ssrv.URL, "/api/admin/session/audit", nil, time.Second),
	}})

	token := h.token(t, []string{"viewer"}, nil)
	var got []int64
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("audit walk did not terminate")
		}
		path := "/api/admin/dashboard/audit?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		resp := h.do(t, h.public, http.MethodGet, path, token)
		if resp.status != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", resp.status, resp.body)
		}
		var body struct {
			Entries []struct {
				ID int64 `json:"id"`
			} `json:"entries"`
			NextCursor *string `json:"next_cursor"`
		}
		if err := json.Unmarshal([]byte(resp.body), &body); err != nil {
			t.Fatalf("body is not JSON: %v (%s)", err, resp.body)
		}
		for _, e := range body.Entries {
			got = append(got, e.ID)
		}
		if body.NextCursor == nil {
			break
		}
		cursor = *body.NextCursor
	}

	// config 3 (at +3 s), admin-auth 4 (+2 s), session 1 (+1.5 s), config 1 (+1 s),
	// admin-auth 2 (+0 s).
	want := []int64{3, 4, 1, 1, 2}
	if len(got) != len(want) {
		t.Fatalf("walked %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("walked %v, want %v", got, want)
		}
	}
	if cfgAuth := cf.auth(); !strings.HasPrefix(cfgAuth, "Bearer ") {
		t.Errorf("config feed Authorization = %q, want the caller's token", cfgAuth)
	}
	if adminAuth := af.auth(); !strings.HasPrefix(adminAuth, "Bearer ") {
		t.Errorf("admin-auth feed Authorization = %q, want the caller's token", adminAuth)
	}
	if cf.auth() != af.auth() || cf.auth() != sf.auth() {
		t.Errorf("the feeds saw different tokens: %q, %q, %q", cf.auth(), af.auth(), sf.auth())
	}

	anon := h.do(t, h.public, http.MethodGet, "/api/admin/dashboard/audit", "")
	if anon.status != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401 (%s)", anon.status, anon.body)
	}
}
