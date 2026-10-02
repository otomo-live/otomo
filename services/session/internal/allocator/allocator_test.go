package allocator

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const key = "allocator-session-key-0123456789abcdefghijklm"

// fakeAllocator answers like services/allocator: behind the Session key, with the codes
// of doc 14 §4.2.
func fakeAllocator(t *testing.T, handle func(w http.ResponseWriter, r *http.Request, body map[string]any)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"code":"unauthorized","message":"unknown key"}}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		handle(w, r, body)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, key, nil)
}

func writeError(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"error":{"code":"`+code+`","message":"x","request_id":"r"}}`)
}

func TestAllocate(t *testing.T) {
	c := fakeAllocator(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		if r.Method != http.MethodPost || r.URL.Path != "/internal/allocations" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		if body["party_id"] != "p1" || len(body["player_ids"].([]any)) != 2 {
			t.Errorf("body = %v", body)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"allocation_id":"a1","server_id":"gs-1","status":"reserved","address":"play.example.com","port":27000,"expires_at":"2026-10-01T12:01:00Z","tickets":{"x":"tx","y":"ty"}}`)
	})
	a, err := c.Allocate(t.Context(), "p1", []string{"x", "y"})
	if err != nil {
		t.Fatal(err)
	}
	if a.AllocationID != "a1" || a.Port != 27000 || a.Tickets["y"] != "ty" {
		t.Errorf("allocation = %+v", a)
	}
}

func TestAllocateErrors(t *testing.T) {
	for _, tt := range []struct {
		status int
		code   string
		want   error
	}{
		{503, "no_capacity", ErrNoCapacity},
		{409, "allocation_conflict", ErrConflict},
		{400, "invalid_request", ErrUnexpectedAnswer},
		{500, "internal_error", ErrUnexpectedAnswer},
		{401, "unauthorized", ErrUnexpectedAnswer},
	} {
		c := fakeAllocator(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) { writeError(w, tt.status, tt.code) })
		if _, err := c.Allocate(t.Context(), "p1", []string{"x"}); !errors.Is(err, tt.want) {
			t.Errorf("%d %s = %v, want %v", tt.status, tt.code, err, tt.want)
		}
	}
}

func TestAWrongKeyIsRefused(t *testing.T) {
	c := fakeAllocator(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) { w.WriteHeader(201) })
	c.key = "not-the-key"
	if _, err := c.Allocate(t.Context(), "p1", []string{"x"}); err == nil {
		t.Error("a wrong key was accepted")
	}
}

// TestACallHasADeadline: a hung Allocator is a failure, not a hang (doc 14 §6).
func TestACallHasADeadline(t *testing.T) {
	c := fakeAllocator(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Allocate(ctx, "p1", []string{"x"}); err == nil {
		t.Fatal("a hung call returned no error")
	}
	if time.Since(start) > 2*time.Second {
		t.Error("the call did not stop at its deadline")
	}
}

func TestTicketAndGet(t *testing.T) {
	c := fakeAllocator(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		switch r.URL.Path {
		case "/internal/allocations/a1/tickets":
			if body["player_id"] != "x" {
				t.Errorf("ticket body = %v", body)
			}
			_, _ = io.WriteString(w, `{"ticket":"tx","expires_at":"2026-10-01T12:01:00Z"}`)
		case "/internal/allocations/a1":
			_, _ = io.WriteString(w, `{"allocation_id":"a1","party_id":"p1","status":"ended","end_reason":"ended"}`)
		case "/internal/allocations/gone/tickets":
			writeError(w, 409, "allocation_ended")
		default:
			writeError(w, 404, "not_found")
		}
	})
	ticket, exp, err := c.Ticket(t.Context(), "a1", "x")
	if err != nil || ticket != "tx" || exp.IsZero() {
		t.Errorf("Ticket = %q %v %v", ticket, exp, err)
	}
	if _, _, err := c.Ticket(t.Context(), "gone", "x"); !errors.Is(err, ErrAllocationEnded) {
		t.Errorf("ended allocation = %v", err)
	}
	s, err := c.Get(t.Context(), "a1")
	if err != nil || s.Live() || s.EndReason != "ended" {
		t.Errorf("Get = %+v %v", s, err)
	}
	if _, err := c.Get(t.Context(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing allocation = %v", err)
	}
}

func TestUnconfiguredClient(t *testing.T) {
	if _, err := New("", "", nil).Allocate(t.Context(), "p", nil); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("unconfigured = %v", err)
	}
}

func TestTicketExpiry(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1800000000,"sub":"x"}`))
	if got := TicketExpiry("h." + payload + ".s"); !got.Equal(time.Unix(1800000000, 0)) {
		t.Errorf("TicketExpiry = %v", got)
	}
	for _, bad := range []string{"", "a.b", "a.!!!.c", "a." + base64.RawURLEncoding.EncodeToString([]byte(`{}`)) + ".c"} {
		if got := TicketExpiry(bad); !got.IsZero() {
			t.Errorf("TicketExpiry(%q) = %v, want zero", bad, got)
		}
	}
}

func TestLive(t *testing.T) {
	for status, want := range map[string]bool{"reserved": true, "active": true, "ended": false, "expired": false} {
		if got := (Status{Status: status}).Live(); got != want {
			t.Errorf("Live(%s) = %v", status, got)
		}
	}
}
