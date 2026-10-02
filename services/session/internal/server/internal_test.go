package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/otomo-live/otomo/services/session/internal/api"
	"github.com/otomo-live/otomo/services/session/internal/servicekey"
)

const (
	testPartyID = "018f4a3e-1c2d-7abc-8def-0123456789a0"
	testAllocID = "018f4a3e-1c2d-7abc-8def-0123456789a1"
	callbackURL = "/internal/session/allocations/" + testAllocID + "/ended"
)

// returnCall is one call a fakeReturner saw.
type returnCall struct{ party, alloc, reason, source string }

// fakeReturner records calls and reports every party as already returned after the
// first, as the conditional store update does.
type fakeReturner struct {
	mu    sync.Mutex
	calls []returnCall
	err   error
}

func (f *fakeReturner) Return(_ context.Context, party, alloc, reason, source string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, returnCall{party, alloc, reason, source})
	return len(f.calls) == 1, f.err
}

// callbackKey writes a generated session_allocator.key and returns the accepted key set
// and the key's text.
func callbackKey(t *testing.T) (*servicekey.Keys, string) {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	text := base64.RawURLEncoding.EncodeToString(raw)
	path := filepath.Join(t.TempDir(), "session_allocator.key")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := servicekey.LoadKeys(map[servicekey.Role]string{servicekey.RoleAllocator: path})
	if err != nil {
		t.Fatal(err)
	}
	return keys, text
}

// post sends a POST with body to base+path, with key as a bearer token when set.
func post(t *testing.T, h *harness, base, path, key, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestTheCallbackIsOnlyOnTheInternalListener is LB-4's first criterion: the callback
// answers on SESSION_INTERNAL_ADDR with session_allocator.key, and is not a route on the
// public or the metrics listener.
func TestTheCallbackIsOnlyOnTheInternalListener(t *testing.T) {
	keys, key := callbackKey(t)
	ret := &fakeReturner{}
	h := newHarness(t, func(d *Deps) {
		d.CallbackKeys = keys
		d.Handlers = &api.Handlers{Returns: ret}
	})
	internal := "http://" + h.InternalAddr().String()
	body := `{"party_id":"` + testPartyID + `","reason":"server_dead"}`

	if code, b := post(t, h, internal, callbackURL, key, body); code != http.StatusNoContent {
		t.Fatalf("callback = %d %s, want 204", code, b)
	}
	if len(ret.calls) != 1 || ret.calls[0] != (returnCall{testPartyID, testAllocID, "server_dead", "callback"}) {
		t.Errorf("returner calls = %+v", ret.calls)
	}

	// Idempotent: the party is already back, and the answer is still 204.
	if code, _ := post(t, h, internal, callbackURL, key, body); code != http.StatusNoContent {
		t.Errorf("repeated callback = %d, want 204", code)
	}

	// Not on the public listener: 401 without a token, 404 with a player's.
	if code, _ := post(t, h, h.publicURL, callbackURL, "", body); code != http.StatusUnauthorized {
		t.Errorf("callback on the public listener with no token = %d, want 401", code)
	}
	if code, _ := post(t, h, h.publicURL, callbackURL, h.playerToken(t), body); code != http.StatusNotFound {
		t.Errorf("callback on the public listener with a player token = %d, want 404", code)
	}
	if code, _ := post(t, h, h.publicURL, callbackURL, key, body); code == http.StatusNoContent {
		t.Error("the public listener accepted the callback key")
	}
	// Not on the metrics listener either.
	if code, _ := post(t, h, h.metricsURL, callbackURL, key, body); code != http.StatusNotFound {
		t.Errorf("callback on the metrics listener = %d, want 404", code)
	}
	if len(ret.calls) != 2 {
		t.Errorf("%d returner calls, want only the two on the internal listener", len(ret.calls))
	}
}

func TestTheInternalListenerNeedsTheKeyOnEveryPath(t *testing.T) {
	keys, key := callbackKey(t)
	_, stranger := callbackKey(t)
	ret := &fakeReturner{}
	h := newHarness(t, func(d *Deps) {
		d.CallbackKeys = keys
		d.Handlers = &api.Handlers{Returns: ret}
	})
	internal := "http://" + h.InternalAddr().String()
	body := `{"party_id":"` + testPartyID + `","reason":"ended"}`

	for _, tt := range []struct {
		name, path, key string
		want            int
	}{
		{"no key", callbackURL, "", http.StatusUnauthorized},
		{"a wrong key", callbackURL, stranger, http.StatusUnauthorized},
		{"no key on an unknown path", "/nope", "", http.StatusUnauthorized},
		{"no key on /metrics", "/metrics", "", http.StatusUnauthorized},
		{"the key on an unknown path", "/nope", key, http.StatusNotFound},
	} {
		code, b := post(t, h, internal, tt.path, tt.key, body)
		if code != tt.want {
			t.Errorf("%s: %d, want %d", tt.name, code, tt.want)
		}
		if code >= 400 {
			errorCode(t, b)
		}
	}
	if len(ret.calls) != 0 {
		t.Errorf("the returner was called without the key: %+v", ret.calls)
	}
}

func TestWithoutACallbackKeyEveryInternalCallIs401(t *testing.T) {
	_, key := callbackKey(t)
	h := newHarness(t, func(d *Deps) { d.Handlers = &api.Handlers{Returns: &fakeReturner{}} })
	body := `{"party_id":"` + testPartyID + `","reason":"ended"}`
	if code, _ := post(t, h, "http://"+h.InternalAddr().String(), callbackURL, key, body); code != http.StatusUnauthorized {
		t.Errorf("callback with no keys configured = %d, want 401", code)
	}
}

func TestTheCallbackRefusesABodyOutsideTheContract(t *testing.T) {
	keys, key := callbackKey(t)
	ret := &fakeReturner{}
	h := newHarness(t, func(d *Deps) {
		d.CallbackKeys = keys
		d.Handlers = &api.Handlers{Returns: ret}
	})
	internal := "http://" + h.InternalAddr().String()

	for _, tt := range []struct{ name, path, body, code string }{
		{"not json", callbackURL, `nope`, "invalid_body"},
		{"no party", callbackURL, `{"reason":"ended"}`, "invalid_request"},
		{"a party that is not a uuid", callbackURL, `{"party_id":"p1","reason":"ended"}`, "invalid_request"},
		{"an allocation that is not a uuid", "/internal/session/allocations/a1/ended", `{"party_id":"` + testPartyID + `"}`, "invalid_request"},
	} {
		code, b := post(t, h, internal, tt.path, key, tt.body)
		if code != http.StatusBadRequest || errorCode(t, b) != tt.code {
			t.Errorf("%s: %d %s, want 400 %s", tt.name, code, b, tt.code)
		}
	}

	// A field the Allocator adds later is ignored, not refused.
	extra := `{"party_id":"` + testPartyID + `","reason":"ended","ended_at":"2026-09-29T00:00:00Z"}`
	if code, b := post(t, h, internal, callbackURL, key, extra); code != http.StatusNoContent {
		t.Errorf("an extra field = %d %s, want 204", code, b)
	}
}

// TestAStoreFailureIsRetried: a 5xx makes the Allocator retry the callback.
func TestAStoreFailureIsRetried(t *testing.T) {
	keys, key := callbackKey(t)
	h := newHarness(t, func(d *Deps) {
		d.CallbackKeys = keys
		d.Handlers = &api.Handlers{Returns: &fakeReturner{err: io.ErrUnexpectedEOF}}
	})
	body := `{"party_id":"` + testPartyID + `","reason":"ended"}`
	if code, _ := post(t, h, "http://"+h.InternalAddr().String(), callbackURL, key, body); code != http.StatusInternalServerError {
		t.Errorf("callback with the store down = %d, want 500", code)
	}
}
