package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/config/internal/api"
	"github.com/otomo-live/otomo/services/config/internal/store"
)

// TestDraftRoutesThroughTheLiveServer is the end-to-end wiring check: the guard
// enforces the viewer/live_ops split, the live_ops PUT reaches the real handler and
// database, and the same PUT replayed at the consumed revision is a 409.
func TestDraftRoutesThroughTheLiveServer(t *testing.T) {
	db := openTestDB(t)

	h := newHarness(t, nil, &api.Handlers{
		Store: db,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	name := fmt.Sprintf("srv.draft_%d", time.Now().UnixNano())
	actor := store.Entry{ActorID: "staff-1", ActorName: "Draft Operator"}
	if _, err := db.CreateNamespace(t.Context(), name, "client", "", actor); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}

	base := "/api/admin/config/namespaces/" + name + "/draft"
	viewer := h.token(t, []string{"viewer"}, nil)
	liveOps := h.token(t, []string{"live_ops"}, nil)

	resp := h.do(t, h.public, http.MethodGet, base, viewer)
	if resp.status != http.StatusOK {
		t.Fatalf("viewer GET = %d, want 200 (%s)", resp.status, resp.body)
	}
	var got struct {
		Namespace   string          `json:"namespace"`
		Document    json.RawMessage `json:"document"`
		Revision    int             `json:"revision"`
		BaseVersion *int            `json:"base_version"`
	}
	if err := json.Unmarshal([]byte(resp.body), &got); err != nil {
		t.Fatalf("GET body is not JSON: %v (%s)", err, resp.body)
	}
	if got.Namespace != name || got.Revision != 1 || string(got.Document) != "{}" || got.BaseVersion != nil {
		t.Errorf("GET = %s r%d %s base=%v, want %s r1 {} base=nil",
			got.Namespace, got.Revision, got.Document, got.BaseVersion, name)
	}

	if resp := h.doJSON(t, http.MethodPut, base, viewer, `{"document":{"level":1},"revision":1}`); resp.status != http.StatusForbidden {
		t.Fatalf("viewer PUT = %d, want 403 (%s)", resp.status, resp.body)
	}

	resp = h.doJSON(t, http.MethodPut, base, liveOps, `{"document":{"level":1},"revision":1}`)
	if resp.status != http.StatusOK {
		t.Fatalf("live_ops PUT = %d, want 200 (%s)", resp.status, resp.body)
	}
	var saved struct {
		Revision int `json:"revision"`
	}
	if err := json.Unmarshal([]byte(resp.body), &saved); err != nil {
		t.Fatalf("PUT body is not JSON: %v (%s)", err, resp.body)
	}
	if saved.Revision != 2 {
		t.Errorf("PUT revision = %d, want 2", saved.Revision)
	}

	resp = h.doJSON(t, http.MethodPut, base, liveOps, `{"document":{"level":1},"revision":1}`)
	if resp.status != http.StatusConflict {
		t.Fatalf("replayed PUT = %d, want 409 (%s)", resp.status, resp.body)
	}
	if code := resp.code(t); code != "stale_revision" {
		t.Errorf("replayed PUT code = %q, want stale_revision", code)
	}
	// CFG-B3: the 409 carries the current draft.
	var conflict struct {
		Error struct {
			Details struct {
				Draft struct {
					Revision int             `json:"revision"`
					Document json.RawMessage `json:"document"`
				} `json:"draft"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(resp.body), &conflict); err != nil {
		t.Fatalf("409 body is not JSON: %v (%s)", err, resp.body)
	}
	if got := conflict.Error.Details.Draft.Revision; got != 2 {
		t.Errorf("409 details.draft.revision = %d, want 2 (%s)", got, resp.body)
	}
	if len(conflict.Error.Details.Draft.Document) == 0 {
		t.Errorf("409 details.draft.document missing (%s)", resp.body)
	}
}
