package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/allocator/internal/metrics"
	"github.com/otomo-live/otomo/services/allocator/internal/pool"
	"github.com/otomo-live/otomo/services/allocator/internal/servicekey"
)

const (
	serversPath   = "/internal/servers"
	registerPath  = "/internal/servers/register"
	heartbeatPath = "/internal/servers/gs-alpha/heartbeat"
	endedPath     = "/internal/servers/gs-alpha/ended"

	validRegisterBody = `{"server_id":"gs-alpha","internal_addr":"10.0.0.1:27015","capacity":8}`
	validEndedBody    = `{"allocation_id":"11111111-1111-1111-1111-111111111111"}`
)

// fakeStore lets a test decide what the registry returns without a database. A nil
// function means "not called on this path", so the auth and decode tests can leave it
// empty.
type fakeStore struct {
	register    func(ctx context.Context, reg pool.Registration) error
	heartbeat   func(ctx context.Context, serverID string, playersConnected int) (*pool.Assignment, error)
	ended       func(ctx context.Context, serverID, allocationID string) error
	listServers func(ctx context.Context) ([]pool.ServerInfo, error)
}

func (f *fakeStore) Register(ctx context.Context, reg pool.Registration) error {
	if f.register == nil {
		return nil
	}
	return f.register(ctx, reg)
}

func (f *fakeStore) Heartbeat(ctx context.Context, serverID string, playersConnected int) (*pool.Assignment, error) {
	if f.heartbeat == nil {
		return nil, nil
	}
	return f.heartbeat(ctx, serverID, playersConnected)
}

func (f *fakeStore) Ended(ctx context.Context, serverID, allocationID string) error {
	if f.ended == nil {
		return nil
	}
	return f.ended(ctx, serverID, allocationID)
}

func (f *fakeStore) ListServers(ctx context.Context) ([]pool.ServerInfo, error) {
	if f.listServers == nil {
		return nil, nil
	}
	return f.listServers(ctx)
}

type harness struct {
	handler http.Handler
	session string
	game    string
	proxy   string
	// metrics is set only by the allocation harness; the game-server routes have no
	// instrument to assert, so their harness leaves it nil and the nil-safe methods
	// record nothing.
	metrics *metrics.Metrics
}

func newHarness(t *testing.T, store ServerStore) *harness {
	t.Helper()

	session, game, proxy := keyText(1), keyText(101), keyText(201)
	keys, err := servicekey.LoadKeys(map[servicekey.Role]string{
		servicekey.RoleSession:    writeKeyFile(t, "session", session),
		servicekey.RoleGameServer: writeKeyFile(t, "game", game),
		servicekey.RoleProxy:      writeKeyFile(t, "proxy", proxy),
	})
	if err != nil {
		t.Fatalf("LoadKeys: %v", err)
	}

	mux := http.NewServeMux()
	RegisterServerRoutes(mux, keys, store)
	return &harness{handler: mux, session: session, game: game, proxy: proxy}
}

// keyText builds a distinct, valid-length secret per seed.
func keyText(seed byte) string {
	b := make([]byte, 32)
	for i := range b {
		b[i] = seed + byte(i)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func writeKeyFile(t *testing.T, name, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return path
}

func (h *harness) post(t *testing.T, path, token, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func (h *harness) get(t *testing.T, path, token string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// envelope is the COM-5 error shape, decoded so a test can assert both the code and
// that the message names the field at fault.
type envelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeEnvelope(t *testing.T, body string) envelope {
	t.Helper()
	var env envelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("body is not the COM-5 envelope: %v (%s)", err, body)
	}
	if env.Error.Message == "" {
		t.Error("error envelope has no message")
	}
	return env
}

// TestRegistryRoutesRequireGameServerKey checks all three routes reject an anonymous
// caller with 401 and the Session or proxy key with 403: the caller is known but not
// allowed here.
func TestRegistryRoutesRequireGameServerKey(t *testing.T) {
	h := newHarness(t, &fakeStore{})

	for _, path := range []string{registerPath, heartbeatPath, endedPath} {
		t.Run(path+"/anonymous", func(t *testing.T) {
			status, body := h.post(t, path, "", "{}")
			if status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (%s)", status, body)
			}
			if code := decodeEnvelope(t, body).Error.Code; code != "unauthorized" {
				t.Errorf("code = %q, want unauthorized", code)
			}
		})
		t.Run(path+"/session key", func(t *testing.T) {
			status, body := h.post(t, path, h.session, "{}")
			if status != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (%s)", status, body)
			}
			if code := decodeEnvelope(t, body).Error.Code; code != "forbidden" {
				t.Errorf("code = %q, want forbidden", code)
			}
		})
		t.Run(path+"/proxy key", func(t *testing.T) {
			status, body := h.post(t, path, h.proxy, "{}")
			if status != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (%s)", status, body)
			}
			if code := decodeEnvelope(t, body).Error.Code; code != "forbidden" {
				t.Errorf("code = %q, want forbidden", code)
			}
		})
	}
}

// TestListServersRequiresProxyKey checks the pool read's auth: the proxy's key is
// admitted, an anonymous or unknown caller is 401, and both existing roles are 403.
func TestListServersRequiresProxyKey(t *testing.T) {
	h := newHarness(t, &fakeStore{})

	cases := []struct {
		name       string
		token      string
		wantStatus int
		wantCode   string
	}{
		{"anonymous", "", http.StatusUnauthorized, "unauthorized"},
		{"unknown key", keyText(77), http.StatusUnauthorized, "unauthorized"},
		{"session key", h.session, http.StatusForbidden, "forbidden"},
		{"gameserver key", h.game, http.StatusForbidden, "forbidden"},
		{"proxy key", h.proxy, http.StatusOK, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := h.get(t, serversPath, tc.token)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", status, tc.wantStatus, body)
			}
			if tc.wantCode != "" {
				if code := decodeEnvelope(t, body).Error.Code; code != tc.wantCode {
					t.Errorf("code = %q, want %q", code, tc.wantCode)
				}
			}
		})
	}
}

// TestListServersReturnsThePool pins the wire shape: 200 with every server rendered as
// its id, address and state.
func TestListServersReturnsThePool(t *testing.T) {
	h := newHarness(t, &fakeStore{listServers: func(context.Context) ([]pool.ServerInfo, error) {
		return []pool.ServerInfo{
			{ServerID: "gs-1", InternalAddr: "gs-1:7777", State: "free"},
			{ServerID: "gs-2", InternalAddr: "gs-2:7778", State: "busy"},
		}, nil
	}})

	status, body := h.get(t, serversPath, h.proxy)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", status, body)
	}

	var resp serverListResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, body)
	}
	want := []pool.ServerInfo{
		{ServerID: "gs-1", InternalAddr: "gs-1:7777", State: "free"},
		{ServerID: "gs-2", InternalAddr: "gs-2:7778", State: "busy"},
	}
	if !slices.Equal(resp.Servers, want) {
		t.Errorf("servers = %+v, want %+v", resp.Servers, want)
	}
}

// TestListServersEmptyPoolIsArray pins the empty case: an empty pool must be `[]`, not
// null, so a proxy iterating the list never needs a nil check.
func TestListServersEmptyPoolIsArray(t *testing.T) {
	h := newHarness(t, &fakeStore{listServers: func(context.Context) ([]pool.ServerInfo, error) {
		return nil, nil
	}})

	status, body := h.get(t, serversPath, h.proxy)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", status, body)
	}
	if got := strings.TrimSpace(body); got != `{"servers":[]}` {
		t.Errorf("body = %q, want {\"servers\":[]}", got)
	}
}

// TestListServersStoreErrorIsInternal checks a registry failure goes through the shared
// error helper and is not leaked to the proxy.
func TestListServersStoreErrorIsInternal(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := newHarness(t, &fakeStore{listServers: func(context.Context) ([]pool.ServerInfo, error) {
		return nil, errors.New("boom: postgres connection refused")
	}})

	status, body := h.get(t, serversPath, h.proxy)
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", status, body)
	}
	if code := decodeEnvelope(t, body).Error.Code; code != "internal_error" {
		t.Errorf("code = %q, want internal_error", code)
	}
	if strings.Contains(body, "boom") {
		t.Errorf("response leaked the internal error: %s", body)
	}
}

func TestRegisterServerReturns204(t *testing.T) {
	var got pool.Registration
	h := newHarness(t, &fakeStore{register: func(_ context.Context, reg pool.Registration) error {
		got = reg
		return nil
	}})

	status, body := h.post(t, registerPath, h.game, validRegisterBody)
	if status != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (%s)", status, body)
	}
	if got.ServerID != "gs-alpha" || got.InternalAddr != "10.0.0.1:27015" || got.Capacity != 8 {
		t.Errorf("store got %+v, want the request body", got)
	}
	if body != "" {
		t.Errorf("204 body = %q, want empty", body)
	}
}

// TestRegisterRejectsMalformedBodies covers the decode and validation failures, all of
// which must be the same 400 invalid_request so a caller has one thing to check.
func TestRegisterRejectsMalformedBodies(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		message string // substring the message must name, "" to skip
	}{
		{"unknown field", `{"server_id":"gs-alpha","internal_addr":"10.0.0.1:27015","capacity":8,"extra":1}`, "extra"},
		{"bad json", `{"server_id":`, ""},
		{"empty body", ``, ""},
		{"oversize", `{"server_id":"` + strings.Repeat("a", maxRequestBodyBytes) + `"}`, ""},
		{"bad server id", `{"server_id":"GS","internal_addr":"10.0.0.1:27015","capacity":8}`, "server_id"},
		{"bad internal addr", `{"server_id":"gs-alpha","internal_addr":"10.0.0.1","capacity":8}`, "internal_addr"},
		{"bad capacity", `{"server_id":"gs-alpha","internal_addr":"10.0.0.1:27015","capacity":65}`, "capacity"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, &fakeStore{})
			status, body := h.post(t, registerPath, h.game, tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", status, body)
			}
			env := decodeEnvelope(t, body)
			if env.Error.Code != "invalid_request" {
				t.Errorf("code = %q, want invalid_request", env.Error.Code)
			}
			if tc.message != "" && !strings.Contains(env.Error.Message, tc.message) {
				t.Errorf("message %q does not name %q", env.Error.Message, tc.message)
			}
		})
	}
}

func TestHeartbeatRejectsBadPlayerCount(t *testing.T) {
	h := newHarness(t, &fakeStore{})

	status, body := h.post(t, heartbeatPath, h.game, `{"players_connected":1001}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", status, body)
	}
	env := decodeEnvelope(t, body)
	if env.Error.Code != "invalid_request" || !strings.Contains(env.Error.Message, "players_connected") {
		t.Errorf("envelope = %+v, want invalid_request naming players_connected", env)
	}
}

func TestEndedRejectsBadAllocationID(t *testing.T) {
	h := newHarness(t, &fakeStore{})

	status, body := h.post(t, endedPath, h.game, `{"allocation_id":"not-a-uuid"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", status, body)
	}
	env := decodeEnvelope(t, body)
	if env.Error.Code != "invalid_request" || !strings.Contains(env.Error.Message, "allocation_id") {
		t.Errorf("envelope = %+v, want invalid_request naming allocation_id", env)
	}
}

// TestHeartbeatReturnsNullAllocation pins the shape: no allocation is an explicit JSON
// null, not an omitted key.
func TestHeartbeatReturnsNullAllocation(t *testing.T) {
	h := newHarness(t, &fakeStore{heartbeat: func(context.Context, string, int) (*pool.Assignment, error) {
		return nil, nil
	}})

	status, body := h.post(t, heartbeatPath, h.game, `{"players_connected":0}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", status, body)
	}
	if got := strings.TrimSpace(body); got != `{"allocation":null}` {
		t.Errorf("body = %q, want {\"allocation\":null}", got)
	}
}

func TestHeartbeatReturnsAssignment(t *testing.T) {
	expires := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	allocID := "11111111-1111-1111-1111-111111111111"
	h := newHarness(t, &fakeStore{heartbeat: func(_ context.Context, serverID string, players int) (*pool.Assignment, error) {
		if serverID != "gs-alpha" {
			t.Errorf("store got server %q, want the path's gs-alpha", serverID)
		}
		if players != 2 {
			t.Errorf("store got %d players, want 2", players)
		}
		return &pool.Assignment{
			AllocationID: allocID,
			PlayerIDs:    []string{"22222222-2222-2222-2222-222222222222"},
			ExpiresAt:    expires,
		}, nil
	}})

	status, body := h.post(t, heartbeatPath, h.game, `{"players_connected":2}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", status, body)
	}

	var resp heartbeatResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, body)
	}
	if resp.Allocation == nil {
		t.Fatalf("allocation = null, want the assignment (%s)", body)
	}
	if resp.Allocation.AllocationID != allocID {
		t.Errorf("allocation_id = %q, want %q", resp.Allocation.AllocationID, allocID)
	}
	if len(resp.Allocation.PlayerIDs) != 1 {
		t.Errorf("player_ids = %v, want one id", resp.Allocation.PlayerIDs)
	}
	if !resp.Allocation.ExpiresAt.Equal(expires) {
		t.Errorf("expires_at = %s, want %s", resp.Allocation.ExpiresAt, expires)
	}
}

// TestStoreErrorsMapToStatus drives every sentinel through the handlers and checks the
// HTTP status and code the contract names.
func TestStoreErrorsMapToStatus(t *testing.T) {
	cases := []struct {
		name       string
		store      ServerStore
		path, body string
		wantStatus int
		wantCode   string
	}{
		{
			"register not registered",
			&fakeStore{register: func(context.Context, pool.Registration) error { return pool.ErrNotRegistered }},
			registerPath, validRegisterBody, http.StatusNotFound, "not_registered",
		},
		{
			"heartbeat not registered",
			&fakeStore{heartbeat: func(context.Context, string, int) (*pool.Assignment, error) {
				return nil, pool.ErrNotRegistered
			}},
			heartbeatPath, `{"players_connected":0}`, http.StatusNotFound, "not_registered",
		},
		{
			"ended not registered",
			&fakeStore{ended: func(context.Context, string, string) error { return pool.ErrNotRegistered }},
			endedPath, validEndedBody, http.StatusNotFound, "not_registered",
		},
		{
			"ended mismatch",
			&fakeStore{ended: func(context.Context, string, string) error { return pool.ErrAllocationMismatch }},
			endedPath, validEndedBody, http.StatusConflict, "allocation_mismatch",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.store)
			status, body := h.post(t, tc.path, h.game, tc.body)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", status, tc.wantStatus, body)
			}
			if code := decodeEnvelope(t, body).Error.Code; code != tc.wantCode {
				t.Errorf("code = %q, want %q", code, tc.wantCode)
			}
		})
	}
}

// TestStoreFailureIsInternalErrorWithoutLeak checks the catch-all: an unexpected error
// is logged, not returned, and the body carries neither the error nor its text.
func TestStoreFailureIsInternalErrorWithoutLeak(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := newHarness(t, &fakeStore{register: func(context.Context, pool.Registration) error {
		return errors.New("boom: postgres connection refused")
	}})

	status, body := h.post(t, registerPath, h.game, validRegisterBody)
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", status, body)
	}
	if code := decodeEnvelope(t, body).Error.Code; code != "internal_error" {
		t.Errorf("code = %q, want internal_error", code)
	}
	if strings.Contains(body, "boom") {
		t.Errorf("response leaked the internal error: %s", body)
	}
}
