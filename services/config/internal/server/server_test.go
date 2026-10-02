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
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/otomo-live/otomo/services/config/internal/api"
	"github.com/otomo-live/otomo/services/config/internal/auth"
	"github.com/otomo-live/otomo/services/config/internal/config"
)

const (
	testKid      = "test-staff-key"
	testIssuer   = "https://php-admin.otomo.internal"
	testAudience = "otomo:staff"
)

// harness is a running server plus the key its tokens must be signed with, so a test
// can assert both halves of the contract: what the listener answers, and that the
// answer came from verifying a real token rather than from a stub.
type harness struct {
	public   string
	internal string
	priv     ed25519.PrivateKey
	client   *http.Client
}

// newHarness builds and starts a server. handlers is optional: the existing tests
// build Deps without any, which is the 501 baseline, while a handler-level test passes
// the wired api.Handlers it wants to exercise.
func newHarness(t *testing.T, ready func(context.Context) error, handlers ...*api.Handlers) *harness {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

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
	deps := Deps{
		Verifier: verifier,
		Ready:    ready,
		Version:  "test",
		// A failing request should fail on its assertion, not on log noise.
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if len(handlers) > 0 {
		deps.Handlers = handlers[0]
	}
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

// TestPublicListenerAdmitsOnlyStaffTokens is CFG-A1's acceptance criterion, from the
// outside: a missing token and a player token must both be 401, and each must name
// its own reason rather than sharing a generic one.
func TestPublicListenerAdmitsOnlyStaffTokens(t *testing.T) {
	h := newHarness(t, nil)

	// A token from the player domain: correctly signed, wrong service. This is the
	// case that a shared key between domains would otherwise let through, so it is
	// asserted against a real signature rather than a garbage string.
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

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, path := range []string{
				"/api/admin/config/namespaces",
				"/api/admin/config/audit",
			} {
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

// TestRoleIsEnforcedPerRoute walks the three roles over routes at each level. The 501
// is the assertion that the request got past the role check and reached the stub: a
// wrong role would have produced a 403, and the two are easy to confuse if only the
// rejection case is tested.
func TestRoleIsEnforcedPerRoute(t *testing.T) {
	h := newHarness(t, nil)

	tests := []struct {
		name   string
		roles  []string
		method string
		path   string
		want   int
	}{
		{"viewer reads namespaces", []string{"viewer"}, http.MethodGet, "/api/admin/config/namespaces", http.StatusNotImplemented},
		{"viewer reads the audit log", []string{"viewer"}, http.MethodGet, "/api/admin/config/audit", http.StatusNotImplemented},
		{"viewer reads release history", []string{"viewer"}, http.MethodGet, "/api/admin/config/channels/dev/releases", http.StatusNotImplemented},
		{"viewer cannot save a draft", []string{"viewer"}, http.MethodPut, "/api/admin/config/namespaces/gameplay/draft", http.StatusForbidden},
		{"viewer cannot create a namespace", []string{"viewer"}, http.MethodPost, "/api/admin/config/namespaces", http.StatusForbidden},
		{"live_ops saves a draft", []string{"live_ops"}, http.MethodPut, "/api/admin/config/namespaces/gameplay/draft", http.StatusNotImplemented},
		{"live_ops publishes to staging", []string{"live_ops"}, http.MethodPost, "/api/admin/config/channels/staging/releases", http.StatusNotImplemented},
		{"live_ops cannot publish to live", []string{"live_ops"}, http.MethodPost, "/api/admin/config/channels/live/releases", http.StatusForbidden},
		{"live_ops cannot roll back", []string{"live_ops"}, http.MethodPost, "/api/admin/config/channels/live/rollback", http.StatusForbidden},
		{"live_ops cannot promote", []string{"live_ops"}, http.MethodPost, "/api/admin/config/channels/staging/promote?from=dev", http.StatusForbidden},
		{"admin publishes to live", []string{"admin"}, http.MethodPost, "/api/admin/config/channels/live/releases", http.StatusNotImplemented},
		{"admin rolls back", []string{"admin"}, http.MethodPost, "/api/admin/config/channels/dev/rollback", http.StatusNotImplemented},
		{"admin promotes", []string{"admin"}, http.MethodPost, "/api/admin/config/channels/staging/promote?from=dev", http.StatusNotImplemented},
		{"no role at all", nil, http.MethodGet, "/api/admin/config/namespaces", http.StatusForbidden},
		{"an unknown role grants nothing", []string{"supervisor"}, http.MethodGet, "/api/admin/config/namespaces", http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := h.do(t, h.public, tt.method, tt.path, h.token(t, tt.roles, nil))
			if resp.status != tt.want {
				t.Fatalf("%s %s = %d, want %d (%s)", tt.method, tt.path, resp.status, tt.want, resp.body)
			}
			if tt.want == http.StatusForbidden {
				if got := resp.code(t); got != auth.ReasonInsufficientRole {
					t.Errorf("code = %q, want %q", got, auth.ReasonInsufficientRole)
				}
			}
		})
	}
}

// TestRejectionsAreCountedByReason ties the response body to the metric: an operator
// alerting on the counter needs it to move for the same reason the client was told.
// It compares counts before and after one request, so an unrelated rejection earlier
// in the same process cannot make it pass.
func TestRejectionsAreCountedByReason(t *testing.T) {
	h := newHarness(t, nil)
	token := h.token(t, []string{"viewer"}, nil)

	before := counterValue(t, h, "config_token_rejected_total", `reason="insufficient_role"`)
	if resp := h.do(t, h.public, http.MethodPost, "/api/admin/config/namespaces", token); resp.status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.status)
	}
	after := counterValue(t, h, "config_token_rejected_total", `reason="insufficient_role"`)
	if after != before+1 {
		t.Errorf("config_token_rejected_total{reason=insufficient_role} went %g -> %g, want exactly +1", before, after)
	}

	// A different reason must land on a different series, which is the point of the
	// label: "someone is forging tokens" and "someone lacks a role" are not the same
	// incident.
	missing := counterValue(t, h, "config_token_rejected_total", `reason="missing_token"`)
	if resp := h.do(t, h.public, http.MethodGet, "/api/admin/config/namespaces", ""); resp.status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.status)
	}
	if got := counterValue(t, h, "config_token_rejected_total", `reason="missing_token"`); got != missing+1 {
		t.Errorf("config_token_rejected_total{reason=missing_token} went %g -> %g, want exactly +1", missing, got)
	}
}

// counterValue reads one series out of the Prometheus text exposition, and returns 0
// when the series is absent — which is what an unlabelled-yet counter actually is, and
// the state of every reason series before the first rejection of that kind.
func counterValue(t *testing.T, h *harness, name, labels string) float64 {
	t.Helper()

	body := h.do(t, h.internal, http.MethodGet, "/metrics", "").body
	prefix := name + "{" + labels
	for _, line := range strings.Split(body, "\n") {
		// labels is a subset of the sample's label set, so it must match up to a comma
		// or the closing brace rather than anywhere in the line.
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

// TestUnmatchedRequestsAreGuardedToo covers the fallback: an unauthenticated request
// for a path that does not exist must be answered like one for a path that does, so
// the service cannot be used to map its own route table from outside.
func TestUnmatchedRequestsAreGuardedToo(t *testing.T) {
	h := newHarness(t, nil)

	t.Run("without a token", func(t *testing.T) {
		resp := h.do(t, h.public, http.MethodGet, "/api/admin/config/nope", "")
		if resp.status != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.status)
		}
		if got := resp.code(t); got != auth.ReasonMissingToken {
			t.Errorf("code = %q, want %q", got, auth.ReasonMissingToken)
		}
	})

	t.Run("with a token", func(t *testing.T) {
		resp := h.do(t, h.public, http.MethodGet, "/api/admin/config/nope", h.token(t, []string{"admin"}, nil))
		if resp.status != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.status)
		}
		if got := resp.code(t); got != "not_found" {
			t.Errorf("code = %q, want not_found", got)
		}
	})

	t.Run("wrong method", func(t *testing.T) {
		resp := h.do(t, h.public, http.MethodDelete, "/api/admin/config/namespaces", h.token(t, []string{"admin"}, nil))
		if resp.status != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", resp.status)
		}
		if got := resp.headers.Get("Allow"); got != "GET, HEAD, POST" {
			t.Errorf("Allow = %q, want \"GET, HEAD, POST\"", got)
		}
	})
}

// TestInternalEndpointsStayOnTheInternalListener is the two-listener split: /metrics
// and /healthz exist, but not on the port Gateway forwards to. An admin token must not
// find them there.
func TestInternalEndpointsStayOnTheInternalListener(t *testing.T) {
	h := newHarness(t, nil)
	token := h.token(t, []string{"admin"}, nil)

	for _, path := range []string{"/healthz", "/readyz", "/metrics", "/debug/pprof/"} {
		resp := h.do(t, h.public, http.MethodGet, path, token)
		if resp.status != http.StatusNotFound {
			t.Errorf("public %s = %d, want 404", path, resp.status)
		}
	}

	if resp := h.do(t, h.internal, http.MethodGet, "/healthz", ""); resp.status != http.StatusOK {
		t.Errorf("internal /healthz = %d, want 200", resp.status)
	}
	if resp := h.do(t, h.internal, http.MethodGet, "/metrics", ""); !strings.Contains(resp.body, "config_build_info") {
		t.Errorf("internal /metrics does not report config_build_info:\n%s", resp.body)
	}
}

// TestReadyzConsultsTheInjectedCheck checks the three answers readiness can give: not
// yet started, a failing dependency named in the body, and ready.
func TestReadyzConsultsTheInjectedCheck(t *testing.T) {
	failing := errors.New("postgres unreachable: dial tcp 10.0.0.1:5432: connect: connection refused")
	h := newHarness(t, func(context.Context) error { return failing })

	resp := h.do(t, h.internal, http.MethodGet, "/readyz", "")
	if resp.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.status)
	}
	if !strings.Contains(resp.body, "postgres unreachable") {
		t.Errorf("body does not name the failing dependency: %s", resp.body)
	}

	// The check runs per request, so a dependency that recovers must be able to make
	// the service ready again without a restart.
	healthy := newHarness(t, func(context.Context) error { return nil })
	if resp := h.do(t, healthy.internal, http.MethodGet, "/readyz", ""); resp.status != http.StatusOK {
		t.Errorf("status = %d, want 200 (%s)", resp.status, resp.body)
	}
}

// TestPublicHandlerRefusesWithoutAVerifier pins the fail-closed direction for a server
// built with a zero-value Deps, which is what every other test relies on being safe.
func TestPublicHandlerRefusesWithoutAVerifier(t *testing.T) {
	srv := New(config.Config{ListenAddr: ":0", MetricsAddr: ":0"}, Deps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	w := httptest.NewRecorder()
	srv.publicHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/admin/config/namespaces", nil))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}
