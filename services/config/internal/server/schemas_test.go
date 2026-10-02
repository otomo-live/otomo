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
	"github.com/otomo-live/otomo/services/config/internal/store"
)

// doJSONHeaders is doJSON with extra request headers, for the schema PUT's If-Match
// precondition.
func (h *harness) doJSONHeaders(t *testing.T, method, path, token, body string, headers map[string]string) response {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+h.public+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return response{status: resp.StatusCode, headers: resp.Header, body: string(raw)}
}

// TestSchemaRoutesThroughTheLiveServer is the end-to-end wiring check: the guard
// enforces the viewer/admin split, the admin PUT reaches the real handler and database,
// and hostile documents are rejected before a version is written.
func TestSchemaRoutesThroughTheLiveServer(t *testing.T) {
	db := openTestDB(t)

	h := newHarness(t, nil, &api.Handlers{
		Store: db,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	name := fmt.Sprintf("srv.schema_%d", time.Now().UnixNano())
	actor := store.Entry{ActorID: "staff-1", ActorName: "Schema Admin"}
	if _, err := db.CreateNamespace(t.Context(), name, "client", "", actor); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}

	base := "/api/admin/config/namespaces/" + name + "/schema"
	viewer := h.token(t, []string{"viewer"}, nil)
	admin := h.token(t, []string{"admin"}, nil)

	resp := h.do(t, h.public, http.MethodGet, base, viewer)
	if resp.status != http.StatusOK {
		t.Fatalf("viewer GET = %d, want 200 (%s)", resp.status, resp.body)
	}
	if etag := resp.headers.Get("ETag"); etag != `"1"` {
		t.Errorf("GET ETag = %q, want %q", etag, `"1"`)
	}
	var got struct {
		Namespace     string          `json:"namespace"`
		SchemaVersion int             `json:"schema_version"`
		Schema        json.RawMessage `json:"schema"`
	}
	if err := json.Unmarshal([]byte(resp.body), &got); err != nil {
		t.Fatalf("GET body is not JSON: %v (%s)", err, resp.body)
	}
	if got.Namespace != name || got.SchemaVersion != 1 || string(got.Schema) != "{}" {
		t.Errorf("GET = %s v%d %s, want %s v1 {}", got.Namespace, got.SchemaVersion, got.Schema, name)
	}

	liveOps := h.token(t, []string{"live_ops"}, nil)
	if resp := h.doJSON(t, http.MethodPut, base, liveOps, `{"type":"object"}`); resp.status != http.StatusForbidden {
		t.Fatalf("live_ops PUT = %d, want 403 (%s)", resp.status, resp.body)
	}

	resp = h.doJSONHeaders(t, http.MethodPut, base, admin, `{"type":"object","properties":{"level":{"type":"integer"}}}`, map[string]string{"If-Match": `"1"`})
	if resp.status != http.StatusOK {
		t.Fatalf("admin PUT = %d, want 200 (%s)", resp.status, resp.body)
	}
	if err := json.Unmarshal([]byte(resp.body), &got); err != nil {
		t.Fatalf("PUT body is not JSON: %v (%s)", err, resp.body)
	}
	if got.SchemaVersion != 2 {
		t.Errorf("after PUT schema_version = %d, want 2", got.SchemaVersion)
	}

	resp = h.doJSONHeaders(t, http.MethodPut, base, admin, `{"type":12}`, map[string]string{"If-Match": `"2"`})
	if resp.status != http.StatusBadRequest {
		t.Fatalf("invalid schema PUT = %d, want 400 (%s)", resp.status, resp.body)
	}
	if code := resp.code(t); code != "validation_failed" {
		t.Errorf("invalid schema code = %q, want validation_failed", code)
	}
	// The message must locate the problem, not just say the schema is invalid.
	if !strings.Contains(resp.body, "at /type:") {
		t.Errorf("invalid schema message does not locate /type: %s", resp.body)
	}

	resp = h.doJSONHeaders(t, http.MethodPut, base, admin, `{"$ref":"file:///etc/passwd"}`, map[string]string{"If-Match": `"2"`})
	if resp.status != http.StatusBadRequest {
		t.Fatalf("file ref PUT = %d, want 400 (%s)", resp.status, resp.body)
	}
	if strings.Contains(resp.body, "root:") {
		t.Errorf("file ref response leaked file contents: %s", resp.body)
	}

	unknown := "/api/admin/config/namespaces/" + fmt.Sprintf("srv.missing_%d", time.Now().UnixNano()) + "/schema"
	if resp := h.doJSONHeaders(t, http.MethodPut, unknown, admin, `{"type":"object"}`, map[string]string{"If-Match": `"1"`}); resp.status != http.StatusNotFound {
		t.Fatalf("unknown namespace PUT = %d, want 404 (%s)", resp.status, resp.body)
	}
}

// TestSchemaPreconditionsAndConflictThroughTheLiveServer covers the optimistic-locking
// contract at the HTTP boundary: a missing precondition is 428, If-Match and
// base_version are equivalent spellings, a conflicting or weak pair is 400, and a stale
// version is 409 carrying the winning schema.
func TestSchemaPreconditionsAndConflictThroughTheLiveServer(t *testing.T) {
	db := openTestDB(t)
	h := newHarness(t, nil, &api.Handlers{
		Store: db,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	name := fmt.Sprintf("srv.schema_pre_%d", time.Now().UnixNano())
	actor := store.Entry{ActorID: "staff-1", ActorName: "Schema Admin"}
	if _, err := db.CreateNamespace(t.Context(), name, "client", "", actor); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}

	base := "/api/admin/config/namespaces/" + name + "/schema"
	admin := h.token(t, []string{"admin"}, nil)
	valid := `{"type":"object"}`

	// Neither header nor query: the version is required, so 428.
	resp := h.doJSON(t, http.MethodPut, base, admin, valid)
	if resp.status != http.StatusPreconditionRequired {
		t.Fatalf("missing precondition = %d, want 428 (%s)", resp.status, resp.body)
	}
	if code := resp.code(t); code != "precondition_required" {
		t.Errorf("missing precondition code = %q, want precondition_required", code)
	}

	// A weak validator is not accepted.
	resp = h.doJSONHeaders(t, http.MethodPut, base, admin, valid, map[string]string{"If-Match": `W/"1"`})
	if resp.status != http.StatusBadRequest {
		t.Fatalf("weak If-Match = %d, want 400 (%s)", resp.status, resp.body)
	}
	if code := resp.code(t); code != "validation_failed" {
		t.Errorf("weak If-Match code = %q, want validation_failed", code)
	}

	// Both spellings present but disagreeing.
	resp = h.doJSONHeaders(t, http.MethodPut, base+"?base_version=2", admin, valid, map[string]string{"If-Match": `"1"`})
	if resp.status != http.StatusBadRequest {
		t.Fatalf("conflicting precondition = %d, want 400 (%s)", resp.status, resp.body)
	}
	if code := resp.code(t); code != "validation_failed" {
		t.Errorf("conflicting precondition code = %q, want validation_failed", code)
	}

	// Bare If-Match works, and GET now advertises v2.
	resp = h.doJSONHeaders(t, http.MethodPut, base, admin, valid, map[string]string{"If-Match": "1"})
	if resp.status != http.StatusOK {
		t.Fatalf("bare If-Match = %d, want 200 (%s)", resp.status, resp.body)
	}

	// base_version is the equivalent query spelling.
	resp = h.doJSON(t, http.MethodPut, base+"?base_version=2", admin, valid)
	if resp.status != http.StatusOK {
		t.Fatalf("base_version = %d, want 200 (%s)", resp.status, resp.body)
	}

	// A stale precondition returns the current schema in details.schema.
	resp = h.doJSONHeaders(t, http.MethodPut, base, admin, valid, map[string]string{"If-Match": `"2"`})
	if resp.status != http.StatusConflict {
		t.Fatalf("stale PUT = %d, want 409 (%s)", resp.status, resp.body)
	}
	if code := resp.code(t); code != "stale_schema" {
		t.Errorf("stale PUT code = %q, want stale_schema", code)
	}
	var env struct {
		Error struct {
			Message string `json:"message"`
			Details struct {
				Schema struct {
					SchemaVersion int `json:"schema_version"`
				} `json:"schema"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(resp.body), &env); err != nil {
		t.Fatalf("stale body is not JSON: %v (%s)", err, resp.body)
	}
	if env.Error.Message != "schema v3 was saved since v2" {
		t.Errorf("stale message = %q, want %q", env.Error.Message, "schema v3 was saved since v2")
	}
	if got := env.Error.Details.Schema.SchemaVersion; got != 3 {
		t.Errorf("stale details.schema.schema_version = %d, want 3", got)
	}

	// GET's ETag tracks the latest version so it can be echoed back.
	getResp := h.do(t, h.public, http.MethodGet, base, admin)
	if getResp.status != http.StatusOK {
		t.Fatalf("GET = %d, want 200 (%s)", getResp.status, getResp.body)
	}
	if etag := getResp.headers.Get("ETag"); etag != `"3"` {
		t.Errorf("GET ETag = %q, want %q", etag, `"3"`)
	}
}
