package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/otomo-live/otomo/services/allocator/internal/metrics"
	"github.com/otomo-live/otomo/services/allocator/internal/pool"
	"github.com/otomo-live/otomo/services/allocator/internal/servicekey"
)

// The allocation fixtures are all valid UUIDs in canonical lowercase, so a test's
// expected value is exactly what the normaliser and Postgres would return.
const (
	allocID      = "0b7e0000-0000-4000-8000-000000000001"
	allocPartyID = "6f1c0000-0000-4000-8000-000000000002"
	allocPlayerA = "a1000000-0000-4000-8000-000000000003"
	allocPlayerB = "b2000000-0000-4000-8000-000000000004"

	allocationsPath       = "/internal/allocations"
	allocationPath        = "/internal/allocations/" + allocID
	allocationTicketsPath = allocationPath + "/tickets"
	latestAllocationPath  = allocationsPath + "?party_id=" + allocPartyID
)

// fakeAllocationStore lets a test decide what the registry returns without a database.
// A nil function means "not called on this path", so the auth and decode tests can
// leave it empty.
type fakeAllocationStore struct {
	allocate     func(ctx context.Context, partyID string, playerIDs []string) (pool.Allocation, bool, error)
	get          func(ctx context.Context, id string) (pool.AllocationInfo, error)
	latest       func(ctx context.Context, partyID string) (pool.AllocationInfo, error)
	ticketTarget func(ctx context.Context, allocationID, playerID string) (string, error)
}

func (f *fakeAllocationStore) Allocate(ctx context.Context, partyID string, playerIDs []string) (pool.Allocation, bool, error) {
	if f.allocate == nil {
		return pool.Allocation{}, false, nil
	}
	return f.allocate(ctx, partyID, playerIDs)
}

func (f *fakeAllocationStore) Get(ctx context.Context, id string) (pool.AllocationInfo, error) {
	if f.get == nil {
		return pool.AllocationInfo{}, nil
	}
	return f.get(ctx, id)
}

func (f *fakeAllocationStore) Latest(ctx context.Context, partyID string) (pool.AllocationInfo, error) {
	if f.latest == nil {
		return pool.AllocationInfo{}, nil
	}
	return f.latest(ctx, partyID)
}

func (f *fakeAllocationStore) TicketTarget(ctx context.Context, allocationID, playerID string) (string, error) {
	if f.ticketTarget == nil {
		return "", nil
	}
	return f.ticketTarget(ctx, allocationID, playerID)
}

// fakeIssuer stands in for token.Issuer: its default is a deterministic ticket so a
// test that does not care about signing still gets a body to inspect.
type fakeIssuer struct {
	issue func(playerID, allocationID, serverID string) (string, time.Time, error)
}

func (f *fakeIssuer) Issue(playerID, allocationID, serverID string) (string, time.Time, error) {
	if f.issue == nil {
		return "ticket-" + playerID, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), nil
	}
	return f.issue(playerID, allocationID, serverID)
}

// newAllocationHarness builds a mux with only the allocation routes mounted, so the
// allocation tests do not depend on the registry routes.
func newAllocationHarness(t *testing.T, store AllocationStore, issuer TicketIssuer) *harness {
	t.Helper()

	session, game := keyText(1), keyText(101)
	keys, err := servicekey.LoadKeys(map[servicekey.Role]string{
		servicekey.RoleSession:    writeKeyFile(t, "session", session),
		servicekey.RoleGameServer: writeKeyFile(t, "game", game),
	})
	if err != nil {
		t.Fatalf("LoadKeys: %v", err)
	}

	mux := http.NewServeMux()
	m := metrics.New(prometheus.NewRegistry(), nil)
	RegisterAllocationRoutes(mux, keys, store, issuer, "play.example.com", 27000, m, nil)
	return &harness{handler: mux, session: session, game: game, metrics: m}
}

// do drives a request through the mux with an optional bearer token and body, so one
// helper covers the GET and POST allocation routes.
func (h *harness) do(t *testing.T, method, path, token, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// allocateBody builds a create request from typed values instead of a hand-written
// string, so a test failure points at the field rather than at a JSON typo.
func allocateBody(partyID string, players ...string) string {
	ids := make([]string, len(players))
	for i, p := range players {
		ids[i] = `"` + p + `"`
	}
	return `{"party_id":"` + partyID + `","player_ids":[` + strings.Join(ids, ",") + `]}`
}

// TestAllocationRoutesRequireSessionKey checks all four routes reject an anonymous
// caller with 401 and a game server's key with 403: the caller is known but not allowed
// on Session's routes.
func TestAllocationRoutesRequireSessionKey(t *testing.T) {
	h := newAllocationHarness(t, &fakeAllocationStore{}, &fakeIssuer{})

	routes := []struct {
		method, path, body string
	}{
		{http.MethodPost, allocationsPath, allocateBody(allocPartyID, allocPlayerA)},
		{http.MethodGet, allocationPath, ""},
		{http.MethodGet, latestAllocationPath, ""},
		{http.MethodPost, allocationTicketsPath, `{"player_id":"` + allocPlayerA + `"}`},
	}

	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path+"/anonymous", func(t *testing.T) {
			status, body := h.do(t, rt.method, rt.path, "", rt.body)
			if status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (%s)", status, body)
			}
			if code := decodeEnvelope(t, body).Error.Code; code != "unauthorized" {
				t.Errorf("code = %q, want unauthorized", code)
			}
		})
		t.Run(rt.method+" "+rt.path+"/gameserver key", func(t *testing.T) {
			status, body := h.do(t, rt.method, rt.path, h.game, rt.body)
			if status != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (%s)", status, body)
			}
			if code := decodeEnvelope(t, body).Error.Code; code != "forbidden" {
				t.Errorf("code = %q, want forbidden", code)
			}
		})
	}
}

// TestAllocateRejectsMalformedBodies covers the decode and validation failures, all of
// which must be the same 400 invalid_request naming the field at fault.
func TestAllocateRejectsMalformedBodies(t *testing.T) {
	nine := make([]string, 9)
	for i := range nine {
		nine[i] = fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
	}

	cases := []struct {
		name  string
		body  string
		field string
	}{
		{
			"unknown field",
			`{"party_id":"` + allocPartyID + `","player_ids":["` + allocPlayerA + `"],"extra":1}`,
			"extra",
		},
		{"bad json", `{"party_id":`, ""},
		{"empty body", ``, ""},
		{"bad party id", `{"party_id":"not-a-uuid","player_ids":["` + allocPlayerA + `"]}`, "party_id"},
		{"no players", allocateBody(allocPartyID), "player_ids"},
		{"nine players", allocateBody(allocPartyID, nine...), "player_ids"},
		{"duplicate players", allocateBody(allocPartyID, strings.ToUpper(allocPlayerA), allocPlayerA), "player_ids"},
		{"bad player id", allocateBody(allocPartyID, "not-a-uuid"), "player_ids"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAllocationHarness(t, &fakeAllocationStore{}, &fakeIssuer{})
			status, body := h.do(t, http.MethodPost, allocationsPath, h.session, tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", status, body)
			}
			env := decodeEnvelope(t, body)
			if env.Error.Code != "invalid_request" {
				t.Errorf("code = %q, want invalid_request", env.Error.Code)
			}
			if tc.field != "" && !strings.Contains(env.Error.Message, tc.field) {
				t.Errorf("message %q does not name %q", env.Error.Message, tc.field)
			}
		})
	}
}

// TestLookupRoutesRejectBadIDs checks the path and query UUID checks on the read and
// ticket routes, including the required party_id on the latest lookup.
func TestLookupRoutesRejectBadIDs(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   string
		field  string
	}{
		{"allocation id", http.MethodGet, "/internal/allocations/not-a-uuid", "", "allocation_id"},
		{"missing party id", http.MethodGet, allocationsPath, "", "party_id"},
		{"bad party id", http.MethodGet, allocationsPath + "?party_id=not-a-uuid", "", "party_id"},
		{"ticket allocation id", http.MethodPost, "/internal/allocations/not-a-uuid/tickets", `{"player_id":"` + allocPlayerA + `"}`, "allocation_id"},
		{"ticket player id", http.MethodPost, allocationTicketsPath, `{"player_id":"not-a-uuid"}`, "player_id"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAllocationHarness(t, &fakeAllocationStore{}, &fakeIssuer{})
			status, body := h.do(t, tc.method, tc.path, h.session, tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", status, body)
			}
			env := decodeEnvelope(t, body)
			if env.Error.Code != "invalid_request" || !strings.Contains(env.Error.Message, tc.field) {
				t.Errorf("envelope = %+v, want invalid_request naming %s", env, tc.field)
			}
		})
	}
}

// TestAllocateReturns201And200 pins the create body and the created/retried split:
// every field comes from the store, the address and port are the configured public
// endpoint, and there is one ticket per normalised player in both cases.
func TestAllocateReturns201And200(t *testing.T) {
	expires := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

	for _, tc := range []struct {
		name       string
		created    bool
		wantStatus int
	}{
		{"new allocation", true, http.StatusCreated},
		{"idempotent retry", false, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotPlayers []string
			h := newAllocationHarness(t,
				&fakeAllocationStore{allocate: func(_ context.Context, partyID string, playerIDs []string) (pool.Allocation, bool, error) {
					if partyID != allocPartyID {
						t.Errorf("store got party %q, want %q", partyID, allocPartyID)
					}
					gotPlayers = playerIDs
					return pool.Allocation{
						AllocationID: allocID,
						ServerID:     "gs-1",
						Status:       "reserved",
						ExpiresAt:    expires,
					}, tc.created, nil
				}},
				&fakeIssuer{issue: func(playerID, allocationID, serverID string) (string, time.Time, error) {
					if allocationID != allocID || serverID != "gs-1" {
						t.Errorf("issuer got allocation %q server %q, want %q and gs-1", allocationID, serverID, allocID)
					}
					return "ticket-" + playerID, expires, nil
				}})

			status, body := h.do(t, http.MethodPost, allocationsPath, h.session,
				allocateBody(allocPartyID, strings.ToUpper(allocPlayerB), allocPlayerA))
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", status, tc.wantStatus, body)
			}

			var resp allocationResponse
			if err := json.Unmarshal([]byte(body), &resp); err != nil {
				t.Fatalf("body is not JSON: %v (%s)", err, body)
			}
			if resp.AllocationID != allocID || resp.ServerID != "gs-1" || resp.Status != "reserved" {
				t.Errorf("response = %+v, want the store's allocation", resp)
			}
			if resp.Address != "play.example.com" || resp.Port != 27000 {
				t.Errorf("address/port = %s:%d, want the public Gameplay Proxy endpoint", resp.Address, resp.Port)
			}
			if !resp.ExpiresAt.Equal(expires) {
				t.Errorf("expires_at = %s, want %s", resp.ExpiresAt, expires)
			}
			if len(resp.Tickets) != 2 {
				t.Fatalf("tickets = %v, want one per player", resp.Tickets)
			}
			for _, playerID := range []string{allocPlayerA, allocPlayerB} {
				if got := resp.Tickets[playerID]; got != "ticket-"+playerID {
					t.Errorf("tickets[%s] = %q, want %q", playerID, got, "ticket-"+playerID)
				}
			}
			if want := []string{allocPlayerA, allocPlayerB}; !slices.Equal(gotPlayers, want) {
				t.Errorf("store got players %v, want normalised %v", gotPlayers, want)
			}
		})
	}
}

// TestGetAllocationReturnsInfo pins the GET body, including the explicit nulls for a
// live allocation's end fields.
func TestGetAllocationReturnsInfo(t *testing.T) {
	created := time.Date(2029, 5, 6, 7, 8, 9, 0, time.UTC)
	info := pool.AllocationInfo{
		AllocationID: allocID,
		PartyID:      allocPartyID,
		ServerID:     "gs-1",
		Status:       "reserved",
		PlayerIDs:    []string{allocPlayerA, allocPlayerB},
		CreatedAt:    created,
		ExpiresAt:    created.Add(time.Minute),
	}

	h := newAllocationHarness(t, &fakeAllocationStore{get: func(_ context.Context, id string) (pool.AllocationInfo, error) {
		if id != allocID {
			t.Errorf("store got id %q, want %q", id, allocID)
		}
		return info, nil
	}}, &fakeIssuer{})

	status, body := h.do(t, http.MethodGet, allocationPath, h.session, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", status, body)
	}

	var got pool.AllocationInfo
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, body)
	}
	if got.AllocationID != allocID || got.PartyID != allocPartyID || got.ServerID != "gs-1" || got.Status != "reserved" {
		t.Errorf("response = %+v, want the store's allocation", got)
	}
	if got.EndReason != nil || got.EndedAt != nil {
		t.Errorf("end_reason/ended_at = %v/%v, want null while live", got.EndReason, got.EndedAt)
	}
	if !slices.Equal(got.PlayerIDs, info.PlayerIDs) {
		t.Errorf("player_ids = %v, want %v", got.PlayerIDs, info.PlayerIDs)
	}
	if !strings.Contains(body, `"end_reason":null`) || !strings.Contains(body, `"ended_at":null`) {
		t.Errorf("body = %s, want explicit null end fields", body)
	}
}

// TestLatestAllocationReturnsInfo pins Session's repair-lookup body: the latest row may
// already be ended, and that is exactly the case the poll exists for.
func TestLatestAllocationReturnsInfo(t *testing.T) {
	endedAt := time.Date(2029, 5, 6, 7, 8, 9, 0, time.UTC)
	reason := "ended"
	info := pool.AllocationInfo{
		AllocationID: allocID,
		PartyID:      allocPartyID,
		ServerID:     "gs-1",
		Status:       "ended",
		EndReason:    &reason,
		PlayerIDs:    []string{allocPlayerA},
		CreatedAt:    endedAt.Add(-time.Minute),
		ExpiresAt:    endedAt,
		EndedAt:      &endedAt,
	}

	h := newAllocationHarness(t, &fakeAllocationStore{latest: func(_ context.Context, partyID string) (pool.AllocationInfo, error) {
		if partyID != allocPartyID {
			t.Errorf("store got party %q, want %q", partyID, allocPartyID)
		}
		return info, nil
	}}, &fakeIssuer{})

	status, body := h.do(t, http.MethodGet, latestAllocationPath, h.session, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", status, body)
	}

	var got pool.AllocationInfo
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, body)
	}
	if got.AllocationID != allocID || got.Status != "ended" {
		t.Errorf("response = %+v, want the ended allocation", got)
	}
	if got.EndReason == nil || *got.EndReason != "ended" || got.EndedAt == nil {
		t.Errorf("end_reason/ended_at = %v/%v, want ended/ended_at set", got.EndReason, got.EndedAt)
	}
}

// TestAllocationTicketReturnsTicket covers the re-issue route: the store sees the
// caller's player id, and the issuer sees it normalised to the canonical lowercase.
func TestAllocationTicketReturnsTicket(t *testing.T) {
	expires := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	var issuedPlayer, issuedAllocation, issuedServer string

	h := newAllocationHarness(t,
		&fakeAllocationStore{ticketTarget: func(_ context.Context, allocationID, playerID string) (string, error) {
			if allocationID != allocID {
				t.Errorf("store got allocation %q, want %q", allocationID, allocID)
			}
			if playerID != strings.ToUpper(allocPlayerA) {
				t.Errorf("store got player %q, want the request's spelling", playerID)
			}
			return "gs-1", nil
		}},
		&fakeIssuer{issue: func(playerID, allocationID, serverID string) (string, time.Time, error) {
			issuedPlayer, issuedAllocation, issuedServer = playerID, allocationID, serverID
			return "signed-ticket", expires, nil
		}})

	status, body := h.do(t, http.MethodPost, allocationTicketsPath, h.session,
		`{"player_id":"`+strings.ToUpper(allocPlayerA)+`"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", status, body)
	}

	var resp ticketResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, body)
	}
	if resp.Ticket != "signed-ticket" || !resp.ExpiresAt.Equal(expires) {
		t.Errorf("response = %+v, want the issuer's ticket and expiry", resp)
	}
	if issuedPlayer != allocPlayerA || issuedAllocation != allocID || issuedServer != "gs-1" {
		t.Errorf("issuer got (%q, %q, %q), want (%q, %q, gs-1)",
			issuedPlayer, issuedAllocation, issuedServer, allocPlayerA, allocID)
	}
}

// TestAllocationStoreErrorsMapToStatus drives every sentinel through the route that
// raises it and checks the HTTP status and code the contract names.
func TestAllocationStoreErrorsMapToStatus(t *testing.T) {
	validBody := allocateBody(allocPartyID, allocPlayerA)
	ticketBody := `{"player_id":"` + allocPlayerA + `"}`

	cases := []struct {
		name       string
		store      AllocationStore
		method     string
		path       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{
			"no capacity",
			&fakeAllocationStore{allocate: func(context.Context, string, []string) (pool.Allocation, bool, error) {
				return pool.Allocation{}, false, pool.ErrNoCapacity
			}},
			http.MethodPost, allocationsPath, validBody, http.StatusServiceUnavailable, "no_capacity",
		},
		{
			"allocation conflict",
			&fakeAllocationStore{allocate: func(context.Context, string, []string) (pool.Allocation, bool, error) {
				return pool.Allocation{}, false, pool.ErrAllocationConflict
			}},
			http.MethodPost, allocationsPath, validBody, http.StatusConflict, "allocation_conflict",
		},
		{
			"not found",
			&fakeAllocationStore{get: func(context.Context, string) (pool.AllocationInfo, error) {
				return pool.AllocationInfo{}, pool.ErrNotFound
			}},
			http.MethodGet, allocationPath, "", http.StatusNotFound, "not_found",
		},
		{
			"latest not found",
			&fakeAllocationStore{latest: func(context.Context, string) (pool.AllocationInfo, error) {
				return pool.AllocationInfo{}, pool.ErrNotFound
			}},
			http.MethodGet, latestAllocationPath, "", http.StatusNotFound, "not_found",
		},
		{
			"allocation ended",
			&fakeAllocationStore{ticketTarget: func(context.Context, string, string) (string, error) {
				return "", pool.ErrAllocationEnded
			}},
			http.MethodPost, allocationTicketsPath, ticketBody, http.StatusConflict, "allocation_ended",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAllocationHarness(t, tc.store, &fakeIssuer{})
			status, body := h.do(t, tc.method, tc.path, h.session, tc.body)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", status, tc.wantStatus, body)
			}
			if code := decodeEnvelope(t, body).Error.Code; code != tc.wantCode {
				t.Errorf("code = %q, want %q", code, tc.wantCode)
			}
		})
	}
}

// TestAllocateRecordsMetrics checks the counter an operations dashboard reads: a fresh
// create, an idempotent retry, and the two expected store failures each land in their
// own result series exactly once.
func TestAllocateRecordsMetrics(t *testing.T) {
	alloc := pool.Allocation{
		AllocationID: allocID,
		ServerID:     "gs-1",
		Status:       "reserved",
		ExpiresAt:    time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC),
	}

	cases := []struct {
		name       string
		store      AllocationStore
		wantStatus int
		wantResult string
	}{
		{
			"created",
			&fakeAllocationStore{allocate: func(context.Context, string, []string) (pool.Allocation, bool, error) {
				return alloc, true, nil
			}},
			http.StatusCreated, metrics.ResultCreated,
		},
		{
			"existing",
			&fakeAllocationStore{allocate: func(context.Context, string, []string) (pool.Allocation, bool, error) {
				return alloc, false, nil
			}},
			http.StatusOK, metrics.ResultExisting,
		},
		{
			"no capacity",
			&fakeAllocationStore{allocate: func(context.Context, string, []string) (pool.Allocation, bool, error) {
				return pool.Allocation{}, false, pool.ErrNoCapacity
			}},
			http.StatusServiceUnavailable, metrics.ResultNoCapacity,
		},
		{
			"conflict",
			&fakeAllocationStore{allocate: func(context.Context, string, []string) (pool.Allocation, bool, error) {
				return pool.Allocation{}, false, pool.ErrAllocationConflict
			}},
			http.StatusConflict, metrics.ResultConflict,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAllocationHarness(t, tc.store, &fakeIssuer{})

			status, body := h.do(t, http.MethodPost, allocationsPath, h.session, allocateBody(allocPartyID, allocPlayerA))
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", status, tc.wantStatus, body)
			}

			if got := testutil.ToFloat64(h.metrics.AllocationCounter(tc.wantResult)); got != 1 {
				t.Errorf("allocations_total{result=%q} = %v, want 1", tc.wantResult, got)
			}
		})
	}
}

// TestAllocationStoreFailureIsInternalErrorWithoutLeak checks the catch-all: an
// unexpected error is logged, not returned, and the body carries neither the error nor
// its text.
func TestAllocationStoreFailureIsInternalErrorWithoutLeak(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := newAllocationHarness(t, &fakeAllocationStore{get: func(context.Context, string) (pool.AllocationInfo, error) {
		return pool.AllocationInfo{}, errors.New("boom: postgres connection refused")
	}}, &fakeIssuer{})

	status, body := h.do(t, http.MethodGet, allocationPath, h.session, "")
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
