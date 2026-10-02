package main_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/otomo-live/otomo/services/gateway_dev/internal/apierr"
	"github.com/otomo-live/otomo/services/gateway_dev/internal/obslog"
	"github.com/otomo-live/otomo/services/gateway_dev/internal/proxy"
	"github.com/otomo-live/otomo/services/gateway_dev/internal/router"
	"github.com/otomo-live/otomo/services/gateway_dev/internal/server"
)

// --- apierr tests ---

func TestWriteError_COM5Shape(t *testing.T) {
	r := httptest.NewRequest("GET", "/test", nil)
	ctx := apierr.WithRequestID(r.Context(), "req-abc-123")
	r = r.WithContext(ctx)

	w := httptest.NewRecorder()
	apierr.WriteError(w, r, http.StatusNotFound, "not_found", "resource not found")

	resp := w.Result()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != "not_found" {
		t.Errorf("code = %q, want not_found", body.Error.Code)
	}
	if body.Error.Message != "resource not found" {
		t.Errorf("message = %q, want resource not found", body.Error.Message)
	}
	if body.Error.RequestID != "req-abc-123" {
		t.Errorf("request_id = %q, want req-abc-123", body.Error.RequestID)
	}
}

// --- Health endpoint tests ---

func TestHealthz_Returns200(t *testing.T) {
	var ready atomic.Bool
	ts := httptest.NewTestServer(t, server.BuildMetricsMux(func(context.Context) bool { return ready.Load() }))
	resp, err := ts.Client().Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", resp.StatusCode)
	}
}

func TestReadyz_Returns503UntilReady(t *testing.T) {
	var ready atomic.Bool
	ts := httptest.NewTestServer(t, server.BuildMetricsMux(func(context.Context) bool { return ready.Load() }))
	client := ts.Client()

	resp, err := client.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("before ready: GET /readyz = %d, want 503", resp.StatusCode)
	}

	ready.Store(true)

	resp, err = client.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz after ready: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("after ready: GET /readyz = %d, want 200", resp.StatusCode)
	}
}

// --- Isolation tests: internal endpoints must NOT be on the public listener ---

func TestPublicListener_InternalEndpointsUnreachable(t *testing.T) {
	ts := httptest.NewTestServer(t, server.BuildPublicHandler(http.NewServeMux(), nil))
	client := ts.Client()

	paths := []string{"/healthz", "/readyz", "/metrics", "/debug/pprof/"}
	for _, path := range paths {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s on public listener = %d, want 404", path, resp.StatusCode)
		}
	}
}

// --- Middleware tests ---

func TestRequestIDMiddleware_GeneratesID(t *testing.T) {
	var capturedID string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = apierr.RequestIDFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	handler := obslog.RequestIDMiddleware(inner)
	ts := httptest.NewTestServer(t, handler)

	resp, err := ts.Client().Get(ts.URL + "/test")
	if err != nil {
		t.Fatal(err)
	}
	if capturedID == "" {
		t.Error("request ID not set on context")
	}
	if got := resp.Header.Get("X-Request-Id"); got != capturedID {
		t.Errorf("response X-Request-Id = %q, context has %q", got, capturedID)
	}
}

func TestRequestIDMiddleware_PreservesInbound(t *testing.T) {
	var capturedID string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = apierr.RequestIDFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	handler := obslog.RequestIDMiddleware(inner)

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Request-Id", "inbound-id-456")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if capturedID != "inbound-id-456" {
		t.Errorf("context request ID = %q, want inbound-id-456", capturedID)
	}
	if got := w.Header().Get("X-Request-Id"); got != "inbound-id-456" {
		t.Errorf("response X-Request-Id = %q, want inbound-id-456", got)
	}
}

func TestRecoverMiddleware_CatchesPanic(t *testing.T) {
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, nil)))
	t.Cleanup(func() { obslog.Init() })

	mux := http.NewServeMux()
	mux.HandleFunc("/boom", func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	})
	handler := server.BuildPublicHandler(mux, nil)
	ts := httptest.NewTestServer(t, handler)

	resp, err := ts.Client().Get(ts.URL + "/boom")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}

	var body struct {
		Error struct {
			Code      string `json:"code"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != "internal_error" {
		t.Errorf("error code = %q, want internal_error", body.Error.Code)
	}
	if body.Error.RequestID == "" {
		t.Fatal("COM-5 body request_id is empty; RequestIDMiddleware did not run before Recover")
	}

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, `"status":500`) {
		t.Errorf("access log missing status 500; LoggingMiddleware did not run after panic recovery\nlog: %s", logOutput)
	}
	if !strings.Contains(logOutput, body.Error.RequestID) {
		t.Errorf("access log does not contain request_id %q\nlog: %s", body.Error.RequestID, logOutput)
	}
}

// --- Shutdown test using synctest for deterministic behaviour ---

func TestGracefulShutdown_InFlightRequestCompletes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requestStarted := make(chan struct{})
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(requestStarted)
			time.Sleep(2 * time.Second)
			w.WriteHeader(http.StatusOK)
		})
		ts := httptest.NewTestServer(t, handler)

		var status int
		done := make(chan struct{})
		go func() {
			defer close(done)
			resp, err := ts.Client().Get(ts.URL + "/slow")
			if err != nil {
				return
			}
			status = resp.StatusCode
			resp.Body.Close()
		}()

		<-requestStarted
		synctest.Wait()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := ts.Config.Shutdown(shutdownCtx); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}

		<-done
		if status != http.StatusOK {
			t.Errorf("in-flight request status = %d, want 200", status)
		}

		// Drain the client transport's idle connection goroutines so
		// synctest does not detect them as a deadlock on bubble exit.
		ts.Client().CloseIdleConnections()
		synctest.Wait()
	})
}

// --- GATE-2 tests ---

// TestServeMuxSpecificity_SpecificRouteWins asserts that the specific route
// POST /api/admin/config/channels/live/releases (RoleAdmin) resolves ahead of
// the general /api/admin/config/ (RoleViewer). A rename silently falling
// through to a lower role requirement is the bug this guards against.
func TestServeMuxSpecificity_SpecificRouteWins(t *testing.T) {
	var matchedMinRole router.Role

	mux := http.NewServeMux()
	for _, route := range router.BuildRoutes() {
		mux.Handle(route.MuxPattern(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			matchedMinRole = route.MinRole
			w.WriteHeader(http.StatusOK)
		}))
	}

	ts := httptest.NewTestServer(t, mux)
	client := ts.Client()
	resp, err := client.Post(ts.URL+"/api/admin/config/channels/live/releases", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if matchedMinRole != router.RoleAdmin {
		t.Errorf("POST .../channels/live/releases matched MinRole=%d, want RoleAdmin (%d)",
			matchedMinRole, router.RoleAdmin)
	}
}

// TestStripPrefix_ForwardsWithTrimmedPath asserts that a route with StripPrefix
// set forwards the trimmed path to the upstream.
func TestStripPrefix_ForwardsWithTrimmedPath(t *testing.T) {
	var receivedPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	reg := proxy.NewRegistry(map[string]*url.URL{"svc": upstreamURL})

	route := router.Route{
		Method: "GET", Pattern: "/prefix/rest",
		Upstream: "svc", StripPrefix: "/prefix",
	}

	mux := http.NewServeMux()
	mux.Handle(route.MuxPattern(), reg.HandlerFor(route))

	ts := httptest.NewTestServer(t, mux)
	resp, err := ts.Client().Get(ts.URL + "/prefix/rest")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if receivedPath != "/rest" {
		t.Errorf("upstream received path %q, want /rest", receivedPath)
	}
}

// TestUnmatchedPath_ReturnsCOM5_404 asserts that an unmatched path returns 404
// in the COM-5 error shape, not ServeMux's default plain-text "404 page not found".
func TestUnmatchedPath_ReturnsCOM5_404(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		apierr.WriteError(w, r, http.StatusNotFound, "not_found", "the requested path does not exist")
	})

	handler := server.BuildPublicHandler(mux, nil)
	ts := httptest.NewTestServer(t, handler)

	resp, err := ts.Client().Get(ts.URL + "/no/such/path")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != "not_found" {
		t.Errorf("error code = %q, want not_found", body.Error.Code)
	}
}

// TestUpstreamDown_ReturnsCOM5_502 asserts that a dead upstream surfaces as 502
// in the COM-5 error shape, not a raw connection reset.
func TestUpstreamDown_ReturnsCOM5_502(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	upstreamURL, _ := url.Parse("http://" + addr)
	reg := proxy.NewRegistry(map[string]*url.URL{"svc": upstreamURL})

	route := router.Route{Method: "*", Pattern: "/api/", Upstream: "svc"}
	mux := http.NewServeMux()
	mux.Handle(route.MuxPattern(), reg.HandlerFor(route))

	ts := httptest.NewTestServer(t, mux)
	resp, err := ts.Client().Get(ts.URL + "/api/anything")
	if err != nil {
		t.Fatalf("GET /api/anything: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != "upstream_error" {
		t.Errorf("error code = %q, want upstream_error", body.Error.Code)
	}
}

// TestStreamRouteHoldsConnection asserts that a Stream: true route holds a
// connection open past GATEWAY_DEV_WRITE_TIMEOUT. Uses synctest for deterministic
// fake time so this is instant rather than a real multi-second sleep.
func TestStreamRouteHoldsConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		upstream := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(5 * time.Second)
			fmt.Fprintf(w, "data: survived\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}))

		upstreamClient := upstream.Client()
		upstreamURL, _ := url.Parse(upstream.URL)
		reg := proxy.NewRegistry(map[string]*url.URL{"session": upstreamURL})
		reg.SetTransport("session", upstreamClient.Transport)

		route := router.Route{
			Method: "*", Pattern: "/events", Upstream: "session",
			Group: router.GroupPlayer, Stream: true,
		}
		mux := http.NewServeMux()
		mux.Handle(route.MuxPattern(), reg.HandlerFor(route))

		gw := httptest.NewTestServer(t, mux)
		gw.Config.WriteTimeout = 1 * time.Second

		resp, err := gw.Client().Get(gw.URL + "/events")
		if err != nil {
			t.Fatalf("GET /events: %v", err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("reading response: %v", err)
		}
		if !strings.Contains(string(body), "data: survived") {
			t.Errorf("stream was cut before upstream finished; body = %q", body)
		}

		gw.Client().CloseIdleConnections()
		reg.CloseIdleConnections()
		synctest.Wait()
	})
}

// --- Forwarded-header tests ---

// TestSetForwarded_ReplacesClientChain asserts that a route with SetForwarded
// discards a client-supplied X-Forwarded-For chain so the upstream sees only the
// peer IP, and that the original scheme is recorded.
func TestSetForwarded_ReplacesClientChain(t *testing.T) {
	var gotXFF, gotProto, gotForwarded, gotRealIP string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXFF = r.Header.Get("X-Forwarded-For")
		gotProto = r.Header.Get("X-Forwarded-Proto")
		gotForwarded = r.Header.Get("Forwarded")
		gotRealIP = r.Header.Get("X-Real-Ip")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	reg := proxy.NewRegistry(map[string]*url.URL{"svc": upstreamURL})

	route := router.Route{
		Method: "GET", Pattern: "/admin-auth/", Upstream: "svc", SetForwarded: true,
	}
	mux := http.NewServeMux()
	mux.Handle(route.MuxPattern(), reg.HandlerFor(route))

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	req, err := http.NewRequest("GET", ts.URL+"/admin-auth/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	req.Header.Set("Forwarded", "for=6.6.6.6")
	req.Header.Set("X-Real-Ip", "6.6.6.6")
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /admin-auth/: %v", err)
	}
	resp.Body.Close()

	if gotForwarded != "" || gotRealIP != "" {
		t.Errorf("client-supplied Forwarded=%q X-Real-Ip=%q reached the upstream", gotForwarded, gotRealIP)
	}
	if gotXFF != "127.0.0.1" {
		t.Errorf("upstream X-Forwarded-For = %q, want 127.0.0.1", gotXFF)
	}
	if gotProto != "http" {
		t.Errorf("upstream X-Forwarded-Proto = %q, want http", gotProto)
	}
}

// TestWithoutSetForwarded_ChainIsAppended asserts that a route without
// SetForwarded keeps today's behaviour: the client chain is preserved and the
// peer IP is appended, with no X-Forwarded-Proto injected.
func TestWithoutSetForwarded_ChainIsAppended(t *testing.T) {
	var gotXFF, gotProto string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXFF = r.Header.Get("X-Forwarded-For")
		gotProto = r.Header.Get("X-Forwarded-Proto")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	reg := proxy.NewRegistry(map[string]*url.URL{"svc": upstreamURL})

	route := router.Route{Method: "GET", Pattern: "/api/", Upstream: "svc"}
	mux := http.NewServeMux()
	mux.Handle(route.MuxPattern(), reg.HandlerFor(route))

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	req, err := http.NewRequest("GET", ts.URL+"/api/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /api/: %v", err)
	}
	resp.Body.Close()

	if gotXFF != "6.6.6.6, 127.0.0.1" {
		t.Errorf("upstream X-Forwarded-For = %q, want %q", gotXFF, "6.6.6.6, 127.0.0.1")
	}
	if gotProto != "" {
		t.Errorf("upstream X-Forwarded-Proto = %q, want empty", gotProto)
	}
}

// TestOnlyAdminAuthRoutesSetForwarded asserts the flag is set on exactly the
// routes served by admin-auth.
func TestOnlyAdminAuthRoutesSetForwarded(t *testing.T) {
	for _, r := range router.BuildRoutes() {
		want := r.Upstream == "adminauth"
		if r.SetForwarded != want {
			t.Errorf("route %s %q upstream %q: SetForwarded = %v, want %v",
				r.Method, r.Pattern, r.Upstream, r.SetForwarded, want)
		}
	}
}

// --- request body cap tests ---

// unknownLengthReader hides a reader's concrete type (and therefore its length)
// from http.NewRequest, so the request goes out chunked with ContentLength -1.
type unknownLengthReader struct{ io.Reader }

func decodeErrorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode COM-5 body: %v", err)
	}
	return body.Error.Code
}

// newBodyCapServer wires one route through the real proxy to a stub upstream.
func newBodyCapServer(t *testing.T, route router.Route, upstream http.Handler) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	upURL, _ := url.Parse(up.URL)
	reg := proxy.NewRegistry(map[string]*url.URL{route.Upstream: upURL})
	t.Cleanup(reg.CloseIdleConnections)
	mux := http.NewServeMux()
	mux.Handle(route.MuxPattern(), reg.HandlerFor(route))
	gw := httptest.NewTestServer(t, mux)
	_ = gw.Client() // start the server and populate URL
	return gw
}

func TestBodyCap_ContentLengthOverLimitIs413(t *testing.T) {
	var called atomic.Bool
	gw := newBodyCapServer(t,
		router.Route{Method: "POST", Pattern: "/api/data/", Upstream: "svc"},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called.Store(true)
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
		}))

	body := bytes.Repeat([]byte("a"), 2<<20) // 2 MiB, Content-Length known
	resp, err := gw.Client().Post(gw.URL+"/api/data/", "application/octet-stream", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if code := decodeErrorCode(t, resp); code != "body_too_large" {
		t.Errorf("error code = %q, want body_too_large", code)
	}
	if called.Load() {
		t.Error("upstream was called for an over-limit body")
	}
}

func TestBodyCap_ChunkedOverLimitIs413(t *testing.T) {
	gw := newBodyCapServer(t,
		router.Route{Method: "POST", Pattern: "/api/data/", Upstream: "svc"},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
		}))

	body := bytes.Repeat([]byte("a"), 2<<20)
	req, err := http.NewRequest("POST", gw.URL+"/api/data/", unknownLengthReader{bytes.NewReader(body)})
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = -1 // force chunked encoding: no Content-Length to pre-check

	resp, err := gw.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if code := decodeErrorCode(t, resp); code != "body_too_large" {
		t.Errorf("error code = %q, want body_too_large", code)
	}
}

func TestBodyCap_UnderLimitPasses(t *testing.T) {
	const size = router.DefaultMaxBody - 1
	received := make(chan int, 1)
	gw := newBodyCapServer(t,
		router.Route{Method: "POST", Pattern: "/api/data/", Upstream: "svc"},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n, _ := io.Copy(io.Discard, r.Body)
			received <- int(n)
			w.WriteHeader(http.StatusOK)
		}))

	resp, err := gw.Client().Post(gw.URL+"/api/data/", "application/octet-stream",
		bytes.NewReader(bytes.Repeat([]byte("a"), size)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if n := <-received; n != size {
		t.Errorf("upstream received %d bytes, want %d", n, size)
	}
}

func TestBodyCap_UploadRouteAllowsLargeBody(t *testing.T) {
	const size = 3 << 20 // 3 MiB, over the default cap but under the packs cap
	received := make(chan int, 1)
	gw := newBodyCapServer(t,
		router.Route{Method: "POST", Pattern: "/api/admin/config/packs", Upstream: "svc",
			MaxBody: 512 << 20, Upload: true},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n, _ := io.Copy(io.Discard, r.Body)
			received <- int(n)
			w.WriteHeader(http.StatusOK)
		}))

	resp, err := gw.Client().Post(gw.URL+"/api/admin/config/packs", "application/octet-stream",
		bytes.NewReader(bytes.Repeat([]byte("a"), size)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if n := <-received; n != size {
		t.Errorf("upstream received %d bytes, want %d", n, size)
	}
}

// TestUploadRouteSurvivesSlowBody proves the Upload flag, not luck, is what
// keeps a stalled upload alive past the server-wide deadlines: the identical
// request on a route without Upload is cut off. synctest makes the 3s stall and
// the 1s timeouts instant and deterministic.
func TestUploadRouteSurvivesSlowBody(t *testing.T) {
	tests := []struct {
		name        string
		upload      bool
		wantSuccess bool
	}{
		{"upload route clears both deadlines", true, true},
		{"non-upload route is cut off", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				received := make(chan []byte, 1)
				upstream := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					b, _ := io.ReadAll(r.Body)
					received <- b
					w.WriteHeader(http.StatusOK)
				}))
				upstreamClient := upstream.Client()
				upstreamURL, _ := url.Parse(upstream.URL)
				reg := proxy.NewRegistry(map[string]*url.URL{"config": upstreamURL})
				reg.SetTransport("config", upstreamClient.Transport)

				route := router.Route{Method: "POST", Pattern: "/api/admin/config/packs", Upstream: "config",
					MaxBody: 512 << 20, Upload: tc.upload}
				mux := http.NewServeMux()
				mux.Handle(route.MuxPattern(), reg.HandlerFor(route))

				gw := httptest.NewTestServer(t, mux)
				gw.Config.ReadTimeout = 1 * time.Second
				gw.Config.WriteTimeout = 1 * time.Second
				client := gw.Client()

				pr, pw := io.Pipe()
				writerDone := make(chan struct{})
				go func() {
					defer close(writerDone)
					_, _ = pw.Write([]byte("first-"))
					time.Sleep(3 * time.Second) // longer than both timeouts
					_, _ = pw.Write([]byte("second"))
					pw.Close()
				}()
				req, err := http.NewRequest("POST", gw.URL+"/api/admin/config/packs", pr)
				if err != nil {
					t.Fatal(err)
				}
				req.ContentLength = -1

				resp, err := client.Do(req)
				if tc.wantSuccess {
					if err != nil {
						t.Fatalf("upload request failed: %v", err)
					}
					defer resp.Body.Close()
					if resp.StatusCode != http.StatusOK {
						t.Fatalf("status = %d, want 200", resp.StatusCode)
					}
					if got := string(<-received); got != "first-second" {
						t.Errorf("upstream received %q, want %q", got, "first-second")
					}
				} else {
					if err == nil && resp.StatusCode == http.StatusOK {
						resp.Body.Close()
						t.Error("non-upload route returned 200 despite stalling past ReadTimeout")
					} else if resp != nil {
						resp.Body.Close()
					}
				}

				<-writerDone
				client.CloseIdleConnections()
				upstream.Client().CloseIdleConnections()
				reg.CloseIdleConnections()
				synctest.Wait()
			})
		})
	}
}

// TestPackUploadRouteShape asserts the real table marks exactly the packs route
// as an upload route, and that ServeMux resolves it ahead of the config prefix
// at the live_ops bar.
func TestPackUploadRouteShape(t *testing.T) {
	var matched router.Route
	mux := http.NewServeMux()
	for _, route := range router.BuildRoutes() {
		if route.Upload || route.MaxBody != 0 {
			if route.Method != "POST" || route.Pattern != "/api/admin/config/packs" {
				t.Errorf("unexpected upload route: %s %s (MaxBody=%d Upload=%v)",
					route.Method, route.Pattern, route.MaxBody, route.Upload)
			}
		}
		mux.Handle(route.MuxPattern(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			matched = route
			w.WriteHeader(http.StatusOK)
		}))
	}

	ts := httptest.NewTestServer(t, mux)
	resp, err := ts.Client().Post(ts.URL+"/api/admin/config/packs", "application/octet-stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if matched.Pattern != "/api/admin/config/packs" {
		t.Fatalf("POST /api/admin/config/packs matched %q", matched.Pattern)
	}
	if matched.MinRole != router.RoleLiveOps {
		t.Errorf("packs MinRole = %d, want RoleLiveOps", matched.MinRole)
	}
	if !matched.Upload {
		t.Error("packs route does not set Upload")
	}
	if matched.MaxBody != 512<<20 {
		t.Errorf("packs MaxBody = %d, want %d", matched.MaxBody, 512<<20)
	}
}
