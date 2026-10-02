package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/otomo-live/otomo/services/config/internal/api"
	"github.com/otomo-live/otomo/services/config/internal/config"
	"github.com/otomo-live/otomo/services/config/internal/store"
	"github.com/otomo-live/otomo/services/config/migrations"
)

// openTestDB opens the shared test database and applies the migrations, skipping the
// whole test when CONFIG_TEST_DATABASE_URL is unset — the same contract the store
// package's DB tests use.
func openTestDB(t *testing.T) *store.DB {
	t.Helper()

	url := os.Getenv("CONFIG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("CONFIG_TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	db, err := store.NewPool(ctx, config.Config{DatabaseURL: url, DBMaxConns: 4})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(db.Close)

	sqlDB, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer sqlDB.Close()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("SetDialect: %v", err)
	}
	if err := goose.Up(sqlDB, "."); err != nil {
		t.Fatalf("goose.Up: %v", err)
	}

	return db
}

// doJSON is do with a request body, for the POST cases the existing harness method
// could not cover.
func (h *harness) doJSON(t *testing.T, method, path, token, body string) response {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+h.public+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
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

// TestNamespacesRoutesThroughTheLiveServer is the end-to-end wiring check: the route's
// role boundary is enforced by the guard and the admin POST reaches the real handler
// and database. A viewer can list, live_ops cannot create, admin can.
func TestNamespacesRoutesThroughTheLiveServer(t *testing.T) {
	db := openTestDB(t)
	const path = "/api/admin/config/namespaces"

	h := newHarness(t, nil, &api.Handlers{
		Store: db,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	name := fmt.Sprintf("srv.ns_%d", time.Now().UnixNano())
	body := fmt.Sprintf(`{"name":%q,"audience":"client"}`, name)

	viewer := h.token(t, []string{"viewer"}, nil)
	if resp := h.do(t, h.public, http.MethodGet, path, viewer); resp.status != http.StatusOK {
		t.Fatalf("viewer GET = %d, want 200 (%s)", resp.status, resp.body)
	}

	liveOps := h.token(t, []string{"live_ops"}, nil)
	if resp := h.doJSON(t, http.MethodPost, path, liveOps, body); resp.status != http.StatusForbidden {
		t.Fatalf("live_ops POST = %d, want 403 (%s)", resp.status, resp.body)
	}

	admin := h.token(t, []string{"admin"}, nil)
	resp := h.doJSON(t, http.MethodPost, path, admin, body)
	if resp.status != http.StatusCreated {
		t.Fatalf("admin POST = %d, want 201 (%s)", resp.status, resp.body)
	}
	var item struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(resp.body), &item); err != nil {
		t.Fatalf("created body is not JSON: %v (%s)", err, resp.body)
	}
	if item.Name != name {
		t.Errorf("created name = %q, want %q", item.Name, name)
	}

	resp = h.do(t, h.public, http.MethodGet, path, viewer)
	if resp.status != http.StatusOK {
		t.Fatalf("viewer GET after create = %d, want 200", resp.status)
	}
	if !strings.Contains(resp.body, name) {
		t.Errorf("list does not contain the created namespace %q:\n%s", name, resp.body)
	}
}
