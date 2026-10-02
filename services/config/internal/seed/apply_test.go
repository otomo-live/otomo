package seed_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path"
	"testing"
	"testing/fstest"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/otomo-live/otomo/services/config/internal/config"
	"github.com/otomo-live/otomo/services/config/internal/seed"
	"github.com/otomo-live/otomo/services/config/internal/store"
	"github.com/otomo-live/otomo/services/config/migrations"
)

// These tests need a real Postgres, because Apply's whole contract — the ordering of
// the schema and draft writes, the skip and the audit rows — is a property of the
// database. They skip when CONFIG_TEST_DATABASE_URL is unset. Namespaces built here
// carry a per-run unique name so they cannot collide with the embedded folder or with
// whatever a previous run left behind in the shared test database.

func applyMigrations(t *testing.T, url string) {
	t.Helper()

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
}

func testDB(t *testing.T) *store.DB {
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

	applyMigrations(t, url)
	return db
}

func uniqueName(kind string) string {
	return fmt.Sprintf("seedtest_%d.%s", time.Now().UnixNano(), kind)
}

const (
	testSchema = `{"$schema":"https://json-schema.org/draft/2020-12/schema","title":"Seed test","description":"A schema for the seed tests.","type":"object","additionalProperties":false,"properties":{"count":{"type":"integer","description":"How many.","minimum":0}}}`
	testDraft  = `{"count":1}`
)

// loadMap builds a one-namespace tree for name and loads it, so the DB tests exercise
// the same Load path a real run does.
func loadMap(t *testing.T, name, audience string) []seed.Namespace {
	t.Helper()

	fsys := fstest.MapFS{
		path.Join(name, "namespace.json"): &fstest.MapFile{
			Data: []byte(fmt.Sprintf(`{"audience":%q,"description":"seed test namespace"}`, audience)),
		},
		path.Join(name, "schema.json"): &fstest.MapFile{Data: []byte(testSchema)},
		path.Join(name, "draft.json"):  &fstest.MapFile{Data: []byte(testDraft)},
	}
	namespaces, err := seed.Load(fsys)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return namespaces
}

// TestApplyCreatedThenSkipped is Apply's core contract: the first run seeds schema v2
// and the seed draft (v1 and the empty draft are CreateNamespace's), the second run
// changes nothing and writes no new audit rows.
func TestApplyCreatedThenSkipped(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	name := uniqueName("flags")
	namespaces := loadMap(t, name, "client")

	report, err := seed.Apply(ctx, db, namespaces, seed.Actor)
	if err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if report.Created() != 1 || report.Skipped() != 0 {
		t.Fatalf("first Apply report = %+v, want 1 created", report.Results)
	}

	got, err := db.LatestSchema(ctx, name)
	if err != nil {
		t.Fatalf("LatestSchema: %v", err)
	}
	if got.SchemaVersion != 2 {
		t.Errorf("schema version = %d, want 2", got.SchemaVersion)
	}
	equalJSON(t, got.Body, []byte(testSchema))

	draft, err := db.Draft(ctx, name)
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}
	if draft.Revision != 2 {
		t.Errorf("draft revision = %d, want 2", draft.Revision)
	}
	equalJSON(t, draft.Body, []byte(testDraft))

	if n := countTargetAudit(t, db, ctx, name); n != 3 {
		t.Errorf("%d audit rows after seeding, want 3 (create, schema.replace, draft.save)", n)
	}

	report, err = seed.Apply(ctx, db, namespaces, seed.Actor)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if report.Created() != 0 || report.Skipped() != 1 {
		t.Errorf("second Apply report = %+v, want 1 skipped", report.Results)
	}
	if n := countTargetAudit(t, db, ctx, name); n != 3 {
		t.Errorf("%d audit rows after the second Apply, want still 3", n)
	}
}

// TestApplyLeavesEditedDraftAlone is the rule that matters most: once a human has
// changed the draft, seeding must not put its own version back.
func TestApplyLeavesEditedDraftAlone(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	name := uniqueName("player")
	namespaces := loadMap(t, name, "client")
	if _, err := seed.Apply(ctx, db, namespaces, seed.Actor); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	before, err := db.Draft(ctx, name)
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}
	edited := []byte(`{"count":42}`)
	if _, err := db.SaveDraft(ctx, name, edited, before.Revision, seed.Actor); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	report, err := seed.Apply(ctx, db, namespaces, seed.Actor)
	if err != nil {
		t.Fatalf("Apply after edit: %v", err)
	}
	if report.Skipped() != 1 {
		t.Errorf("report = %+v, want 1 skipped", report.Results)
	}

	after, err := db.Draft(ctx, name)
	if err != nil {
		t.Fatalf("Draft after: %v", err)
	}
	equalJSON(t, after.Body, edited)
	if after.Revision != before.Revision+1 {
		t.Errorf("draft revision = %d, want %d", after.Revision, before.Revision+1)
	}
}

// TestPlanDoesNotWrite checks that --dry-run's existence probe reports a namespace that
// is absent as creatable and writes nothing.
func TestPlanDoesNotWrite(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	name := uniqueName("dryrun")
	namespaces := loadMap(t, name, "client")

	report, err := seed.Plan(ctx, db, namespaces)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if report.Created() != 1 {
		t.Fatalf("Plan report = %+v, want 1 created", report.Results)
	}
	if _, err := db.LatestSchema(ctx, name); err == nil {
		t.Fatal("Plan created a namespace")
	}
}

// TestApplyEmbeddedFolder applies the real embedded tree, which is the folder a deploy
// actually seeds. The second pass proves it is idempotent even when the namespaces
// already exist, which they will on every deploy after the first.
func TestApplyEmbeddedFolder(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	namespaces, err := seed.Load(seed.FS)
	if err != nil {
		t.Fatalf("Load(embedded): %v", err)
	}

	report, err := seed.Apply(ctx, db, namespaces, seed.Actor)
	if err != nil {
		t.Fatalf("first Apply(embedded): %v", err)
	}
	if len(report.Results) != len(namespaces) {
		t.Fatalf("report has %d results, want %d: %+v", len(report.Results), len(namespaces), report.Results)
	}

	report, err = seed.Apply(ctx, db, namespaces, seed.Actor)
	if err != nil {
		t.Fatalf("second Apply(embedded): %v", err)
	}
	if report.Skipped() != len(namespaces) {
		t.Errorf("second Apply(embedded) report = %+v, want all skipped", report.Results)
	}
}

func countTargetAudit(t *testing.T, db *store.DB, ctx context.Context, target string) int {
	t.Helper()

	var n int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE target = $1`, target).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n
}

// TestApplyFinishesHalfSeededNamespace: a run that died after CreateNamespace left
// schema v1 {} and an untouched draft; the next Apply must finish it, not skip it.
func TestApplyFinishesHalfSeededNamespace(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	name := uniqueName("half")
	namespaces := loadMap(t, name, "client")
	if _, err := db.CreateNamespace(ctx, name, "client", "", seed.Actor); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}

	plan, err := seed.Plan(ctx, db, namespaces)
	if err != nil || plan.Created() != 1 {
		t.Fatalf("Plan = %+v, %v; want 1 created", plan.Results, err)
	}
	report, err := seed.Apply(ctx, db, namespaces, seed.Actor)
	if err != nil || report.Created() != 1 {
		t.Fatalf("Apply = %+v, %v; want 1 created", report.Results, err)
	}
	got, err := db.LatestSchema(ctx, name)
	if err != nil {
		t.Fatalf("LatestSchema: %v", err)
	}
	if got.SchemaVersion != 2 {
		t.Errorf("schema version = %d, want 2", got.SchemaVersion)
	}
	equalJSON(t, got.Body, []byte(testSchema))
	draft, err := db.Draft(ctx, name)
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}
	equalJSON(t, draft.Body, []byte(testDraft))
}
