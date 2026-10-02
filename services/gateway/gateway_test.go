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

	"gateway/gateway/internal/apierr"
	"gateway/gateway/internal/obslog"
	"gateway/gateway/internal/proxy"
	"gateway/gateway/internal/router"
	"gateway/gateway/internal/server"
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

// TestPlayerRoute_OverCapBodyIs413 asserts that the player session route, which
// sets no MaxBody, gets the shared 1 MiB default: an over-cap body is refused
// with a COM-5 413 and never reaches the upstream.
func TestPlayerRoute_OverCapBodyIs413(t *testing.T) {
	var reached atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	var route router.Route
	for _, r := range router.BuildRoutes() {
		if r.Pattern == "/api/player/session/" {
			route = r
		}
	}
	if route.Pattern == "" {
		t.Fatal("router.BuildRoutes() has no /api/player/session/ route")
	}
	if route.MaxBody != 0 {
		t.Fatalf("session route MaxBody = %d, want 0 (the default)", route.MaxBody)
	}

	upstreamURL, _ := url.Parse(upstream.URL)
	reg := proxy.NewRegistry(map[string]*url.URL{"session": upstreamURL})
	mux := http.NewServeMux()
	mux.Handle(route.MuxPattern(), reg.HandlerFor(route))
	ts := httptest.NewTestServer(t, mux)

	body := bytes.Repeat([]byte("x"), router.DefaultMaxBody+1)
	resp, err := ts.Client().Post(ts.URL+"/api/player/session/x", "application/octet-stream", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if e.Error.Code != "body_too_large" {
		t.Errorf("error code = %q, want body_too_large", e.Error.Code)
	}
	if reached.Load() {
		t.Error("over-cap body reached the upstream")
	}
}

// TestStreamRouteHoldsConnection asserts that a Stream: true route holds a
// connection open past GATEWAY_WRITE_TIMEOUT. Uses synctest for deterministic
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

// TestBlobRoute_SlowDownloadSurvivesWriteTimeout is the blob-route counterpart
// to TestStreamRouteHoldsConnection: the real /patch/v1/blob/ route (found in
// the route table, not hand-built) must carry a slow .pck download past
// GATEWAY_WRITE_TIMEOUT. It uses the real route so dropping Stream: true fails
// here.
func TestBlobRoute_SlowDownloadSurvivesWriteTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const sha = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		payload := strings.Repeat("pck!", 4096)

		upstream := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(5 * time.Second)
			fmt.Fprint(w, payload)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}))

		var blobRoute router.Route
		for _, r := range router.BuildRoutes() {
			if r.Pattern == "/patch/v1/blob/" {
				blobRoute = r
			}
		}
		if blobRoute.Pattern == "" {
			t.Fatal("router.BuildRoutes() has no /patch/v1/blob/ route")
		}

		upstreamClient := upstream.Client()
		upstreamURL, _ := url.Parse(upstream.URL)
		reg := proxy.NewRegistry(map[string]*url.URL{"patch": upstreamURL})
		reg.SetTransport("patch", upstreamClient.Transport)

		mux := http.NewServeMux()
		mux.Handle(blobRoute.MuxPattern(), reg.HandlerFor(blobRoute))

		gw := httptest.NewTestServer(t, mux)
		gw.Config.WriteTimeout = 1 * time.Second

		resp, err := gw.Client().Get(gw.URL + "/patch/v1/blob/" + sha)
		if err != nil {
			t.Fatalf("GET /patch/v1/blob/%s: %v", sha, err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("reading response: %v", err)
		}
		if string(body) != payload {
			t.Errorf("slow download cut short: got %d bytes, want %d", len(body), len(payload))
		}

		gw.Client().CloseIdleConnections()
		reg.CloseIdleConnections()
		synctest.Wait()
	})
}
