package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/otomo-live/otomo/services/config/internal/api"
	"github.com/otomo-live/otomo/services/config/internal/blob"
	"github.com/otomo-live/otomo/services/config/internal/store"
)

// stubPackStore is a DB-free api.PackStore for the timing and metric tests. It mirrors
// the real store's content-addressing: a repeated sha256 returns the original row.
type stubPackStore struct {
	mu     sync.Mutex
	bySHA  map[string]store.Pack
	audits int
}

func newStubPackStore() *stubPackStore {
	return &stubPackStore{bySHA: map[string]store.Pack{}}
}

func (s *stubPackStore) CreatePack(_ context.Context, name string, ref blob.Ref, actor store.Entry) (store.Pack, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.bySHA[ref.SHA256]; ok {
		return p, false, nil
	}
	p := store.Pack{
		PackID:     uuid.New().String(),
		Name:       name,
		SHA256:     ref.SHA256,
		Size:       ref.Size,
		UploadedBy: actor.ActorID,
		UploadedAt: time.Unix(0, 0).UTC(),
	}
	s.bySHA[ref.SHA256] = p
	s.audits++
	return p, true, nil
}

func (s *stubPackStore) ListPacks(context.Context) ([]store.Pack, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	packs := make([]store.Pack, 0, len(s.bySHA))
	for _, p := range s.bySHA {
		packs = append(packs, p)
	}
	return packs, nil
}

// TestPackUploadDeadlinesAreCleared is the reason the handler calls
// ResponseController.SetReadDeadline/SetWriteDeadline: a server with a 1s read timeout
// must still accept a pack whose body stalls for 2s, because a 512 MiB upload is far
// longer than any JSON budget.
func TestPackUploadDeadlinesAreCleared(t *testing.T) {
	blobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	handlers := &api.Handlers{
		Blobs:        blobs,
		Packs:        newStubPackStore(),
		MaxPackBytes: 1 << 30,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	mux := http.NewServeMux()
	mux.Handle("POST "+packUploadPath, handlers.For(api.Route{Method: http.MethodPost, Path: packUploadPath}))

	ts := httptest.NewUnstartedServer(mux)
	ts.Config.ReadTimeout = time.Second
	ts.Config.WriteTimeout = time.Second
	ts.Start()
	defer ts.Close()

	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		if _, err := pw.Write([]byte("GDPC")); err != nil {
			return
		}
		time.Sleep(2 * time.Second)
		_, _ = pw.Write([]byte("stalled-body"))
	}()

	req, err := http.NewRequest(http.MethodPost, ts.URL+packUploadPath+"?name=slow_pack", pr)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("POST stalled pack: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", resp.StatusCode, body)
	}
}

// TestPackUploadBytesMetric checks the hook the server wires into the handler: the
// byte counter moves by exactly the uploaded size.
func TestPackUploadBytesMetric(t *testing.T) {
	blobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	h := newHarness(t, nil, &api.Handlers{
		Blobs:        blobs,
		Packs:        newStubPackStore(),
		MaxPackBytes: 1 << 20,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	body := "GDPCmetric-body"
	before := unlabelledCounter(t, h, "config_pack_upload_bytes_total")
	resp := h.doJSON(t, http.MethodPost, packUploadPath+"?name=metric_pack",
		h.token(t, []string{"live_ops"}, nil), body)
	if resp.status != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", resp.status, resp.body)
	}
	after := unlabelledCounter(t, h, "config_pack_upload_bytes_total")
	if after != before+float64(len(body)) {
		t.Errorf("config_pack_upload_bytes_total went %g -> %g, want +%d", before, after, len(body))
	}
}

// unlabelledCounter reads a counter with no labels out of the Prometheus exposition.
func unlabelledCounter(t *testing.T, h *harness, name string) float64 {
	t.Helper()

	for _, line := range strings.Split(h.do(t, h.internal, http.MethodGet, "/metrics", "").body, "\n") {
		if !strings.HasPrefix(line, name+" ") {
			continue
		}
		var v float64
		if _, err := fmt.Sscanf(strings.TrimPrefix(line, name+" "), "%g", &v); err != nil {
			t.Fatalf("cannot parse %q: %v", line, err)
		}
		return v
	}
	return 0
}

// packUploadPath is the external POST path, spelled out so the test does not have to
// reach into the api package's unexported constant.
const packUploadPath = "/api/admin/config/packs"

// countPackBlobFiles counts every regular file under a blob root, which is how the
// tests see that exactly one blob was committed and no temp file survived.
func countPackBlobFiles(t *testing.T, root string) int {
	t.Helper()

	n := 0
	if err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return n
}

type packBody struct {
	PackID     string    `json:"pack_id"`
	Name       string    `json:"name"`
	SHA256     string    `json:"sha256"`
	Size       int64     `json:"size"`
	UploadedBy string    `json:"uploaded_by"`
	UploadedAt time.Time `json:"uploaded_at"`
}

// TestPacksRoutesThroughTheLiveServer is the DB-backed end-to-end check: upload creates
// a row, an audit entry and a blob; replaying the same bytes under a new name returns
// the original 200 row with no second audit or blob; the list contains it; and the role
// split holds.
func TestPacksRoutesThroughTheLiveServer(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	blobs, err := blob.NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	h := newHarness(t, nil, &api.Handlers{
		Store:        db,
		Blobs:        blobs,
		MaxPackBytes: 1 << 20,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	// A body unique to this run, so the sha256 unique index cannot collide with a
	// previous `go test` invocation against the same database.
	body := "GDPC" + uuid.New().String() + uuid.New().String()
	liveOps := h.token(t, []string{"live_ops"}, nil)
	viewer := h.token(t, []string{"viewer"}, nil)

	resp := h.doJSON(t, http.MethodPost, packUploadPath+"?name=first_pack", liveOps, body)
	if resp.status != http.StatusCreated {
		t.Fatalf("live_ops POST = %d, want 201 (%s)", resp.status, resp.body)
	}
	var first packBody
	if err := json.Unmarshal([]byte(resp.body), &first); err != nil {
		t.Fatalf("created body is not JSON: %v (%s)", err, resp.body)
	}
	if first.Name != "first_pack" || first.Size != int64(len(body)) || len(first.SHA256) != 64 {
		t.Errorf("created = %+v, want name first_pack size %d and a 64-char sha", first, len(body))
	}
	if first.PackID == "" || first.UploadedAt.IsZero() {
		t.Errorf("created = %+v, want a pack_id and uploaded_at", first)
	}

	countAudit := func() int {
		t.Helper()
		var n int
		if err := db.Pool.QueryRow(t.Context(),
			`SELECT count(*) FROM audit_log
			  WHERE action = 'pack.upload' AND details->>'sha256' = $1`, first.SHA256).Scan(&n); err != nil {
			t.Fatalf("count pack.upload audit rows: %v", err)
		}
		return n
	}
	if n := countAudit(); n != 1 {
		t.Errorf("%d pack.upload audit rows after the first upload, want 1", n)
	}
	if n := countPackBlobFiles(t, root); n != 1 {
		t.Errorf("%d blob files after the first upload, want 1", n)
	}

	// The same bytes under a different name: 200, the original row, no new audit, no
	// second blob.
	resp = h.doJSON(t, http.MethodPost, packUploadPath+"?name=second_pack", liveOps, body)
	if resp.status != http.StatusOK {
		t.Fatalf("duplicate POST = %d, want 200 (%s)", resp.status, resp.body)
	}
	var second packBody
	if err := json.Unmarshal([]byte(resp.body), &second); err != nil {
		t.Fatalf("duplicate body is not JSON: %v (%s)", err, resp.body)
	}
	if second.PackID != first.PackID || second.Name != first.Name ||
		second.UploadedBy != first.UploadedBy || !second.UploadedAt.Equal(first.UploadedAt) {
		t.Errorf("duplicate row = %+v, want the original %+v", second, first)
	}
	if n := countAudit(); n != 1 {
		t.Errorf("%d pack.upload audit rows after a duplicate, want still 1", n)
	}
	if n := countPackBlobFiles(t, root); n != 1 {
		t.Errorf("%d blob files after a duplicate, want still 1", n)
	}

	// Viewer can list the pack; live_ops is not required for a read.
	resp = h.do(t, h.public, http.MethodGet, packUploadPath, viewer)
	if resp.status != http.StatusOK {
		t.Fatalf("viewer GET = %d, want 200 (%s)", resp.status, resp.body)
	}
	var list struct {
		Packs []packBody `json:"packs"`
	}
	if err := json.Unmarshal([]byte(resp.body), &list); err != nil {
		t.Fatalf("list body is not JSON: %v (%s)", err, resp.body)
	}
	var found bool
	for _, p := range list.Packs {
		if p.PackID == first.PackID {
			found = true
		}
	}
	if !found {
		t.Errorf("list does not contain pack %s: %s", first.PackID, resp.body)
	}

	// A viewer cannot upload.
	if resp := h.doJSON(t, http.MethodPost, packUploadPath+"?name=viewer_pack", viewer, body); resp.status != http.StatusForbidden {
		t.Errorf("viewer POST = %d, want 403 (%s)", resp.status, resp.body)
	}
}
