package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/config/internal/api"
	"github.com/otomo-live/otomo/services/config/internal/schema"
	"github.com/otomo-live/otomo/services/config/internal/store"
)

// weaponsSchema is the schema PUT in the test: a weapons array whose items carry a
// non-negative integer damage. It is the same document the schema package's unit tests
// use, so the pointers asserted here and there cannot drift.
const weaponsSchema = `{
  "type": "object",
  "properties": {
    "weapons": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {"damage": {"type": "integer", "minimum": 0}},
        "required": ["damage"]
      }
    }
  },
  "required": ["weapons"]
}`

// TestDraftValidateRoutesThroughTheLiveServer is the end-to-end wiring check for the
// validate endpoint: the live_ops/viewer split, the schema lookup, the issue pointers,
// and — the reason the endpoint exists — that validating changes nothing.
func TestDraftValidateRoutesThroughTheLiveServer(t *testing.T) {
	db := openTestDB(t)

	h := newHarness(t, nil, &api.Handlers{
		Store:   db,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Schemas: &schema.Cache{},
	})

	name := fmt.Sprintf("srv.validate_%d", time.Now().UnixNano())
	actor := store.Entry{ActorID: "staff-1", ActorName: "Validate Operator"}
	if _, err := db.CreateNamespace(t.Context(), name, "client", "", actor); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}

	admin := h.token(t, []string{"admin"}, nil)
	liveOps := h.token(t, []string{"live_ops"}, nil)
	viewer := h.token(t, []string{"viewer"}, nil)

	schemaPath := "/api/admin/config/namespaces/" + name + "/schema"
	if resp := h.doJSONHeaders(t, http.MethodPut, schemaPath, admin, weaponsSchema, map[string]string{"If-Match": `"1"`}); resp.status != http.StatusOK {
		t.Fatalf("admin schema PUT = %d, want 200 (%s)", resp.status, resp.body)
	}

	base := "/api/admin/config/namespaces/" + name + "/draft/validate"

	// A struct with a raw Errors field lets each case assert both the decoded pointer
	// and that an empty list is [] rather than null.
	type validation struct {
		Valid         bool            `json:"valid"`
		SchemaVersion int             `json:"schema_version"`
		Errors        json.RawMessage `json:"errors"`
	}

	tests := []struct {
		name       string
		document   string
		wantValid  bool
		wantErrors string
	}{
		{"valid document", `{"weapons":[{"damage":3}]}`, true, `[]`},
		{"wrong type", `{"weapons":[{"id":"sword","damage":"lots"}]}`, false, ""},
		{"below minimum", `{"weapons":[{"damage":-1}]}`, false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := h.doJSON(t, http.MethodPost, base, liveOps, `{"document":`+tt.document+`}`)
			if resp.status != http.StatusOK {
				t.Fatalf("POST = %d, want 200 (%s)", resp.status, resp.body)
			}
			var got validation
			if err := json.Unmarshal([]byte(resp.body), &got); err != nil {
				t.Fatalf("body is not JSON: %v (%s)", err, resp.body)
			}
			if got.Valid != tt.wantValid {
				t.Errorf("valid = %v, want %v", got.Valid, tt.wantValid)
			}
			if got.SchemaVersion < 1 {
				t.Errorf("schema_version = %d, want a real version", got.SchemaVersion)
			}
			if tt.wantErrors != "" {
				if string(got.Errors) != tt.wantErrors {
					t.Errorf("errors = %s, want %s", got.Errors, tt.wantErrors)
				}
				return
			}

			var issues []struct {
				Pointer string `json:"pointer"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(got.Errors, &issues); err != nil {
				t.Fatalf("errors is not a list: %v (%s)", err, got.Errors)
			}
			if len(issues) != 1 {
				t.Fatalf("issues = %v, want exactly 1", issues)
			}
			if issues[0].Pointer != "/weapons/0/damage" {
				t.Errorf("pointer = %q, want /weapons/0/damage", issues[0].Pointer)
			}
			if issues[0].Message == "" {
				t.Error("issue has no message")
			}
		})
	}

	// The guard, not the handler, owns this boundary.
	if resp := h.doJSON(t, http.MethodPost, base, viewer, `{"document":{"weapons":[{"damage":3}]}}`); resp.status != http.StatusForbidden {
		t.Errorf("viewer POST = %d, want 403 (%s)", resp.status, resp.body)
	}

	// An unknown namespace is a 404 from the schema lookup, not an empty answer.
	unknown := "/api/admin/config/namespaces/" + fmt.Sprintf("srv.missing_%d", time.Now().UnixNano()) + "/draft/validate"
	if resp := h.doJSON(t, http.MethodPost, unknown, liveOps, `{"document":{}}`); resp.status != http.StatusNotFound {
		t.Errorf("unknown namespace POST = %d, want 404 (%s)", resp.status, resp.body)
	}

	// Nothing was saved: the draft keeps its revision and its content.
	draftPath := "/api/admin/config/namespaces/" + name + "/draft"
	before := h.do(t, h.public, http.MethodGet, draftPath, viewer)
	if before.status != http.StatusOK {
		t.Fatalf("draft GET = %d, want 200 (%s)", before.status, before.body)
	}
	var draft struct {
		Document json.RawMessage `json:"document"`
		Revision int             `json:"revision"`
	}
	if err := json.Unmarshal([]byte(before.body), &draft); err != nil {
		t.Fatalf("draft body is not JSON: %v (%s)", err, before.body)
	}
	if draft.Revision != 1 || strings.TrimSpace(string(draft.Document)) != "{}" {
		t.Errorf("after validating, draft = %s r%d, want {} r1", draft.Document, draft.Revision)
	}
}
