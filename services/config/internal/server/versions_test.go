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
	"github.com/otomo-live/otomo/services/config/internal/blob"
	"github.com/otomo-live/otomo/services/config/internal/schema"
	"github.com/otomo-live/otomo/services/config/internal/store"
)

// TestVersionsRoutesThroughTheLiveServer wires the whole create-version path through a
// real listener: the role split, draft validation with a located pointer, the no-change
// and stale-revision conflicts, and the namespace list reflecting the new version.
func TestVersionsRoutesThroughTheLiveServer(t *testing.T) {
	db := openTestDB(t)
	blobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	h := newHarness(t, nil, &api.Handlers{
		Store:   db,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Schemas: &schema.Cache{},
		Blobs:   blobs,
	})

	name := fmt.Sprintf("srv.version_%d", time.Now().UnixNano())
	actor := store.Entry{ActorID: "staff-1", ActorName: "Version Operator"}
	if _, err := db.CreateNamespace(t.Context(), name, "client", "", actor); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	schemaBody := []byte(`{"type":"object","properties":{"level":{"type":"integer"}},"required":["level"]}`)
	if _, err := db.ReplaceSchema(t.Context(), name, schemaBody, nil, actor); err != nil {
		t.Fatalf("ReplaceSchema: %v", err)
	}
	if _, err := db.SaveDraft(t.Context(), name, []byte(`{"level":1}`), 1, actor); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	base := "/api/admin/config/namespaces/" + name + "/versions"
	viewer := h.token(t, []string{"viewer"}, nil)
	liveOps := h.token(t, []string{"live_ops"}, nil)

	resp := h.doJSON(t, http.MethodPost, base, liveOps, `{"message":"first","revision":2}`)
	if resp.status != http.StatusCreated {
		t.Fatalf("live_ops POST = %d, want 201 (%s)", resp.status, resp.body)
	}
	var created struct {
		Namespace     string `json:"namespace"`
		Version       int    `json:"version"`
		SchemaVersion int    `json:"schema_version"`
		SHA256        string `json:"sha256"`
		Size          int    `json:"size"`
		Message       string `json:"message"`
		CreatedBy     string `json:"created_by"`
	}
	if err := json.Unmarshal([]byte(resp.body), &created); err != nil {
		t.Fatalf("created body is not JSON: %v (%s)", err, resp.body)
	}
	if created.Namespace != name || created.Version != 1 || created.SchemaVersion != 2 {
		t.Errorf("created = %s v%d schema v%d, want %s v1 schema v2",
			created.Namespace, created.Version, created.SchemaVersion, name)
	}
	if len(created.SHA256) != 64 || created.Size <= 0 {
		t.Errorf("created sha256=%q size=%d, want a 64-char digest and a positive size", created.SHA256, created.Size)
	}
	if created.Message != "first" || created.CreatedBy != "staff-1" {
		t.Errorf("created message/by = %q/%q, want first/staff-1", created.Message, created.CreatedBy)
	}

	if resp := h.doJSON(t, http.MethodPost, base, viewer, `{"message":"nope","revision":2}`); resp.status != http.StatusForbidden {
		t.Errorf("viewer POST = %d, want 403 (%s)", resp.status, resp.body)
	}

	resp = h.doJSON(t, http.MethodPost, base, liveOps, `{"message":"   ","revision":2}`)
	if resp.status != http.StatusBadRequest {
		t.Fatalf("empty message = %d, want 400 (%s)", resp.status, resp.body)
	}
	if code := resp.code(t); code != "validation_failed" {
		t.Errorf("empty message code = %q, want validation_failed", code)
	}

	// Make the draft invalid in a way the schema locates, and prove the 400 names the
	// pointer and the schema version.
	if _, err := db.SaveDraft(t.Context(), name, []byte(`{"level":"nope"}`), 2, actor); err != nil {
		t.Fatalf("SaveDraft invalid: %v", err)
	}
	resp = h.doJSON(t, http.MethodPost, base, liveOps, `{"message":"bad","revision":3}`)
	if resp.status != http.StatusBadRequest {
		t.Fatalf("invalid draft = %d, want 400 (%s)", resp.status, resp.body)
	}
	if code := resp.code(t); code != "validation_failed" {
		t.Errorf("invalid draft code = %q, want validation_failed", code)
	}
	if !strings.Contains(resp.body, "/level") {
		t.Errorf("invalid draft message does not name the pointer: %s", resp.body)
	}

	// Restore the draft to the published content so the replay is a no-change.
	if _, err := db.SaveDraft(t.Context(), name, []byte(`{"level":1}`), 3, actor); err != nil {
		t.Fatalf("SaveDraft restore: %v", err)
	}
	resp = h.doJSON(t, http.MethodPost, base, liveOps, `{"message":"again","revision":4}`)
	if resp.status != http.StatusConflict {
		t.Fatalf("no-change replay = %d, want 409 (%s)", resp.status, resp.body)
	}
	if code := resp.code(t); code != "no_changes" {
		t.Errorf("no-change replay code = %q, want no_changes", code)
	}

	resp = h.doJSON(t, http.MethodPost, base, liveOps, `{"message":"stale","revision":2}`)
	if resp.status != http.StatusConflict {
		t.Fatalf("stale revision = %d, want 409 (%s)", resp.status, resp.body)
	}
	if code := resp.code(t); code != "stale_revision" {
		t.Errorf("stale revision code = %q, want stale_revision", code)
	}

	resp = h.do(t, h.public, http.MethodGet, "/api/admin/config/namespaces", viewer)
	if resp.status != http.StatusOK {
		t.Fatalf("viewer GET namespaces = %d, want 200 (%s)", resp.status, resp.body)
	}
	var list struct {
		Namespaces []struct {
			Name   string `json:"name"`
			Latest *int   `json:"latest_version"`
			Draft  struct {
				Revision              int  `json:"revision"`
				HasUnpublishedChanges bool `json:"has_unpublished_changes"`
			} `json:"draft"`
		} `json:"namespaces"`
	}
	if err := json.Unmarshal([]byte(resp.body), &list); err != nil {
		t.Fatalf("namespaces body is not JSON: %v (%s)", err, resp.body)
	}
	var found bool
	for _, ns := range list.Namespaces {
		if ns.Name != name {
			continue
		}
		found = true
		if ns.Latest == nil || *ns.Latest != 1 {
			t.Errorf("latest_version = %v, want 1", ns.Latest)
		}
		if ns.Draft.HasUnpublishedChanges {
			t.Error("has_unpublished_changes = true, want false after restoring the draft")
		}
	}
	if !found {
		t.Errorf("namespaces list does not contain %q", name)
	}
}

// TestVersionHistoryAndDiffThroughTheLiveServer drives the read side of the version
// routes end to end: paging, one version's document and canonical size, a structural
// diff against another version and against the working draft, and the 400/404 edges.
func TestVersionHistoryAndDiffThroughTheLiveServer(t *testing.T) {
	db := openTestDB(t)
	blobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	h := newHarness(t, nil, &api.Handlers{
		Store:   db,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Schemas: &schema.Cache{},
		Blobs:   blobs,
	})

	name := fmt.Sprintf("srv.history_%d", time.Now().UnixNano())
	actor := store.Entry{ActorID: "staff-1", ActorName: "History Operator"}
	if _, err := db.CreateNamespace(t.Context(), name, "client", "", actor); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	if _, err := db.ReplaceSchema(t.Context(), name, []byte(`{"type":"object"}`), nil, actor); err != nil {
		t.Fatalf("ReplaceSchema: %v", err)
	}

	base := "/api/admin/config/namespaces/" + name + "/versions"
	diffBase := "/api/admin/config/namespaces/" + name + "/diff"
	viewer := h.token(t, []string{"viewer"}, nil)
	liveOps := h.token(t, []string{"live_ops"}, nil)

	docs := []string{
		`{"level":1,"weapons":[{"damage":5}],"old":"x"}`,
		`{"level":1,"weapons":[{"damage":6}],"old":"x"}`,
		`{"level":1,"weapons":[{"damage":7}],"old":"x"}`,
	}
	for i, doc := range docs {
		revision := i + 1
		if _, err := db.SaveDraft(t.Context(), name, []byte(doc), revision, actor); err != nil {
			t.Fatalf("SaveDraft %d: %v", revision, err)
		}
		body := fmt.Sprintf(`{"message":"v%d","revision":%d}`, revision, revision+1)
		if resp := h.doJSON(t, http.MethodPost, base, liveOps, body); resp.status != http.StatusCreated {
			t.Fatalf("live_ops POST v%d = %d (%s)", revision, resp.status, resp.body)
		}
	}
	// The draft is then moved past v3 so the 3->draft diff has something to find.
	if _, err := db.SaveDraft(t.Context(), name,
		[]byte(`{"level":2,"weapons":[{"damage":7}],"new":true}`), 4, actor); err != nil {
		t.Fatalf("SaveDraft draft: %v", err)
	}

	type listBody struct {
		Versions []struct {
			Version       int       `json:"version"`
			SchemaVersion int       `json:"schema_version"`
			SHA256        string    `json:"sha256"`
			Message       string    `json:"message"`
			CreatedBy     string    `json:"created_by"`
			CreatedAt     time.Time `json:"created_at"`
		} `json:"versions"`
		NextBefore *int `json:"next_before"`
	}
	list := func(path string) listBody {
		t.Helper()
		resp := h.do(t, h.public, http.MethodGet, path, viewer)
		if resp.status != http.StatusOK {
			t.Fatalf("viewer GET %s = %d (%s)", path, resp.status, resp.body)
		}
		var got listBody
		if err := json.Unmarshal([]byte(resp.body), &got); err != nil {
			t.Fatalf("GET %s body is not JSON: %v (%s)", path, err, resp.body)
		}
		return got
	}

	// Viewer can read the whole history, newest first, with an explicit null cursor.
	got := list(base)
	if len(got.Versions) != 3 || got.Versions[0].Version != 3 || got.Versions[1].Version != 2 || got.Versions[2].Version != 1 {
		t.Fatalf("versions = %+v, want [3 2 1]", got.Versions)
	}
	if got.NextBefore != nil {
		t.Errorf("next_before = %d, want null", *got.NextBefore)
	}
	if got.Versions[0].Message != "v3" || got.Versions[0].CreatedBy != "staff-1" {
		t.Errorf("newest = %+v, want v3 by staff-1", got.Versions[0])
	}

	got = list(base + "?limit=2")
	if len(got.Versions) != 2 || got.Versions[0].Version != 3 || got.Versions[1].Version != 2 {
		t.Fatalf("limit=2 versions = %+v, want [3 2]", got.Versions)
	}
	if got.NextBefore == nil || *got.NextBefore != 2 {
		t.Fatalf("limit=2 next_before = %v, want 2", got.NextBefore)
	}

	got = list(base + "?before=2")
	if len(got.Versions) != 1 || got.Versions[0].Version != 1 {
		t.Fatalf("before=2 versions = %+v, want [1]", got.Versions)
	}
	if got.NextBefore != nil {
		t.Errorf("before=2 next_before = %d, want null", *got.NextBefore)
	}

	// The single-version route returns the canonical size, not the jsonb text length.
	resp := h.do(t, h.public, http.MethodGet, base+"/2", viewer)
	if resp.status != http.StatusOK {
		t.Fatalf("viewer GET /versions/2 = %d (%s)", resp.status, resp.body)
	}
	var detail struct {
		Version  int             `json:"version"`
		Size     int             `json:"size"`
		Document json.RawMessage `json:"document"`
	}
	if err := json.Unmarshal([]byte(resp.body), &detail); err != nil {
		t.Fatalf("GET /versions/2 body is not JSON: %v (%s)", err, resp.body)
	}
	canonical, err := schema.Canonical([]byte(docs[1]))
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	if detail.Version != 2 || detail.Size != len(canonical) {
		t.Errorf("version 2 = v%d size %d, want v2 size %d", detail.Version, detail.Size, len(canonical))
	}
	if string(detail.Document) != string(canonical) {
		t.Errorf("document = %s, want %s", detail.Document, canonical)
	}
	for v := 1; v <= 3; v++ {
		if resp := h.do(t, h.public, http.MethodGet, fmt.Sprintf("%s/%d", base, v), viewer); resp.status != http.StatusOK {
			t.Errorf("viewer GET /versions/%d = %d (%s)", v, resp.status, resp.body)
		}
	}

	// diff 1->3: only the nested damage changed.
	type changeBody struct {
		Op   string          `json:"op"`
		Path string          `json:"path"`
		From json.RawMessage `json:"from"`
		To   json.RawMessage `json:"to"`
	}
	type diffBody struct {
		From struct {
			Ref      string          `json:"ref"`
			Document json.RawMessage `json:"document"`
		} `json:"from"`
		To struct {
			Ref      string          `json:"ref"`
			Document json.RawMessage `json:"document"`
		} `json:"to"`
		Changes []changeBody `json:"changes"`
	}
	resp = h.do(t, h.public, http.MethodGet, diffBase+"?from=1&to=3", viewer)
	if resp.status != http.StatusOK {
		t.Fatalf("diff 1->3 = %d (%s)", resp.status, resp.body)
	}
	var d diffBody
	if err := json.Unmarshal([]byte(resp.body), &d); err != nil {
		t.Fatalf("diff body is not JSON: %v (%s)", err, resp.body)
	}
	if len(d.Changes) != 1 || d.Changes[0].Op != "replace" || d.Changes[0].Path != "/weapons/0/damage" {
		t.Fatalf("diff 1->3 changes = %+v, want one replace at /weapons/0/damage", d.Changes)
	}
	if string(d.Changes[0].From) != "5" || string(d.Changes[0].To) != "7" {
		t.Errorf("diff 1->3 values = %s -> %s, want 5 -> 7", d.Changes[0].From, d.Changes[0].To)
	}
	if d.From.Ref != "1" || d.To.Ref != "3" || len(d.From.Document) == 0 || len(d.To.Document) == 0 {
		t.Errorf("diff 1->3 sides = %+v -> %+v", d.From, d.To)
	}

	// diff 3->draft: replace /level, add /new, remove /old, in sorted key order.
	resp = h.do(t, h.public, http.MethodGet, diffBase+"?from=3&to=draft", viewer)
	if resp.status != http.StatusOK {
		t.Fatalf("diff 3->draft = %d (%s)", resp.status, resp.body)
	}
	d = diffBody{}
	if err := json.Unmarshal([]byte(resp.body), &d); err != nil {
		t.Fatalf("diff body is not JSON: %v (%s)", err, resp.body)
	}
	wantOps := []string{"replace", "add", "remove"}
	wantPaths := []string{"/level", "/new", "/old"}
	if len(d.Changes) != 3 {
		t.Fatalf("diff 3->draft changes = %+v, want 3", d.Changes)
	}
	for i := range wantOps {
		if d.Changes[i].Op != wantOps[i] || d.Changes[i].Path != wantPaths[i] {
			t.Errorf("change %d = %s %s, want %s %s", i, d.Changes[i].Op, d.Changes[i].Path, wantOps[i], wantPaths[i])
		}
	}
	if d.From.Ref != "3" || d.To.Ref != "draft" {
		t.Errorf("diff 3->draft refs = %s -> %s, want 3 -> draft", d.From.Ref, d.To.Ref)
	}
	if string(d.Changes[0].From) != "1" || string(d.Changes[0].To) != "2" {
		t.Errorf("level change = %s -> %s, want 1 -> 2", d.Changes[0].From, d.Changes[0].To)
	}
	if string(d.Changes[1].To) != "true" {
		t.Errorf("new add = %s, want true", d.Changes[1].To)
	}
	if string(d.Changes[2].From) != `"x"` {
		t.Errorf("old remove from = %s, want \"x\"", d.Changes[2].From)
	}

	// Bad paging and ref params are validation_failed, not a silent default.
	badParams := []string{
		base + "?limit=0",
		base + "?limit=201",
		base + "?limit=abc",
		base + "?before=0",
		base + "/0",
		diffBase + "?from=1",
		diffBase + "?from=1&to=abc",
		diffBase + "?from=0&to=draft",
	}
	for _, path := range badParams {
		resp := h.do(t, h.public, http.MethodGet, path, viewer)
		if resp.status != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400 (%s)", path, resp.status, resp.body)
			continue
		}
		if code := resp.code(t); code != "validation_failed" {
			t.Errorf("GET %s code = %q, want validation_failed", path, code)
		}
	}

	// Unknown versions and namespaces are 404.
	missing := fmt.Sprintf("srv.missing_%d", time.Now().UnixNano())
	notFound := []string{
		base + "/99",
		diffBase + "?from=99&to=draft",
		"/api/admin/config/namespaces/" + missing + "/versions",
		"/api/admin/config/namespaces/" + missing + "/versions/1",
		"/api/admin/config/namespaces/" + missing + "/diff?from=1&to=2",
	}
	for _, path := range notFound {
		resp := h.do(t, h.public, http.MethodGet, path, viewer)
		if resp.status != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 (%s)", path, resp.status, resp.body)
		}
	}
}
