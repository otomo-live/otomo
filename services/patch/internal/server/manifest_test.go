package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/otomo-live/otomo/services/patch/internal/manifest"
)

// doWithHeader is harness.do plus one request header, for the If-None-Match
// revalidation request the plain helper cannot express.
func (h *harness) doWithHeader(t *testing.T, base, path, key, value string) response {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+base+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set(key, value)
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return response{status: resp.StatusCode, headers: resp.Header, body: string(body)}
}

// TestManifestRouteServesAndCounts walks the real listener: the route is registered
// when a Holder is present, serves the entry's bytes with the validators, answers a
// matching If-None-Match with 304, and records the two results on separate metric
// series.
func TestManifestRouteServesAndCounts(t *testing.T) {
	body := []byte(`{"release":42}`)
	holder := manifest.NewHolder(manifest.NewSet(map[string]*manifest.Entry{
		"live": {Channel: "live", Body: body, ETag: `"abc"`, MinClientVersion: "2.0.0"},
	}))
	h := newHarnessWithDeps(t, Deps{ManifestHolder: holder})

	resp := h.do(t, h.public, "/patch/v1/live/manifest")
	if resp.status != http.StatusOK {
		t.Fatalf("GET live manifest = %d, want 200 (%s)", resp.status, resp.body)
	}
	if resp.body != string(body) {
		t.Errorf("body = %q, want %q", resp.body, body)
	}
	if got := resp.headers.Get("ETag"); got != `"abc"` {
		t.Errorf("ETag = %q, want %q", got, `"abc"`)
	}

	resp = h.doWithHeader(t, h.public, "/patch/v1/live/manifest", "If-None-Match", `"abc"`)
	if resp.status != http.StatusNotModified {
		t.Fatalf("revalidation = %d, want 304 (%s)", resp.status, resp.body)
	}
	if resp.body != "" {
		t.Errorf("304 body = %q, want empty", resp.body)
	}

	metrics := h.do(t, h.internal, "/metrics").body
	for _, series := range []string{
		`patch_manifest_requests_total{channel="live",result="200"} 1`,
		`patch_manifest_requests_total{channel="live",result="304"} 1`,
	} {
		if !strings.Contains(metrics, series) {
			t.Errorf("/metrics does not contain %q", series)
		}
	}
}

// TestManifestRouteAbsentWithoutHolder pins the zero-value Deps behaviour: with no
// manifests the route is not registered, so a public request falls through to the
// COM-5 catch-all rather than a 503 for a route that does not exist.
func TestManifestRouteAbsentWithoutHolder(t *testing.T) {
	h := newHarness(t, nil, nil)

	resp := h.do(t, h.public, "/patch/v1/live/manifest")
	if resp.status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.status)
	}
	if got := resp.code(t); got != "not_found" {
		t.Errorf("code = %q, want not_found", got)
	}
}

// TestManifestServesBootstrappedLive is the DB-backed end-to-end check: the real
// loader publishes the seeded live release, the listener serves it with an ETag that
// is the channel_head manifest_sha256 quoted, and a revalidation returns 304.
func TestManifestServesBootstrappedLive(t *testing.T) {
	url := os.Getenv("PATCH_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PATCH_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass('public.channel_head') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("check channel_head: %v", err)
	}
	if !exists {
		t.Skip("channel_head does not exist; apply Config's migrations to PATCH_TEST_DATABASE_URL")
	}

	var holder manifest.Holder
	loader := &manifest.Loader{DB: pool, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Holder: &holder}
	if err := loader.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	var sum string
	if err := pool.QueryRow(ctx,
		`SELECT r.manifest_sha256
		   FROM release r JOIN channel_head h USING (release_id)
		  WHERE h.channel = 'live'`).Scan(&sum); err != nil {
		t.Fatalf("read live manifest_sha256: %v", err)
	}
	wantETag := `"` + sum + `"`

	h := newHarnessWithDeps(t, Deps{ManifestHolder: &holder})
	resp := h.do(t, h.public, "/patch/v1/live/manifest")
	if resp.status != http.StatusOK {
		t.Fatalf("GET live manifest = %d, want 200 (%s)", resp.status, resp.body)
	}
	if got := resp.headers.Get("ETag"); got != wantETag {
		t.Errorf("ETag = %q, want %q", got, wantETag)
	}
	if resp.body == "" {
		t.Error("live manifest body is empty")
	}

	resp = h.doWithHeader(t, h.public, "/patch/v1/live/manifest", "If-None-Match", wantETag)
	if resp.status != http.StatusNotModified {
		t.Fatalf("revalidation = %d, want 304 (%s)", resp.status, resp.body)
	}
	if resp.body != "" {
		t.Errorf("304 body = %q, want empty", resp.body)
	}
}
