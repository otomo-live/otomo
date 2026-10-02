package store_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"uuid"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/otomo-live/otomo/services/config/internal/config"
	"github.com/otomo-live/otomo/services/config/internal/store"
	"github.com/otomo-live/otomo/services/config/migrations"
)

// These tests need a real Postgres, because everything they check — goose idempotency,
// the seeded bootstrap releases, the audit row landing in the caller's transaction —
// is a property of the database rather than of this package. They skip when
// CONFIG_TEST_DATABASE_URL is unset, so `go test ./...` stays runnable without one.

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

// TestMigrationsAreIdempotentAndSeedEachChannel is CFG-A2's acceptance criterion. The
// second run is the point: Jenkins runs the migration job before every deploy, so
// "apply twice" has to be a no-op rather than a second set of bootstrap releases.
func TestMigrationsAreIdempotentAndSeedEachChannel(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	applyMigrations(t, os.Getenv("CONFIG_TEST_DATABASE_URL"))

	var tables int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_schema = 'public'
		   AND table_name IN ('config_namespace', 'config_schema', 'config_draft',
		                      'config_version', 'content_pack', 'release',
		                      'channel_head', 'audit_log')`).Scan(&tables); err != nil {
		t.Fatalf("count tables: %v", err)
	}
	if tables != 8 {
		t.Errorf("found %d of the 8 config tables", tables)
	}

	rows, err := db.Pool.Query(ctx, `SELECT channel, release_id FROM channel_head ORDER BY channel`)
	if err != nil {
		t.Fatalf("read channel_head: %v", err)
	}
	defer rows.Close()

	var channels []string
	for rows.Next() {
		var ch string
		var id int64
		if err := rows.Scan(&ch, &id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		channels = append(channels, ch)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(channels) != 3 || channels[0] != "dev" || channels[1] != "live" || channels[2] != "staging" {
		t.Fatalf("channel_head = %v, want exactly [dev live staging]", channels)
	}
}

// TestBootstrapManifestsAreCanonical checks the claim migration 00002 makes in its own
// header: that the hash it stores is the hash the publish path will compute for the
// same document. It recomputes it the way that path will — unmarshal, re-marshal
// (Go sorts map keys), hash the bytes — so a seed written in a different key order or
// with different spacing would fail here rather than at the first cache lookup.
func TestBootstrapManifestsAreCanonical(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	rows, err := db.Pool.Query(ctx,
		`SELECT r.channel, r.manifest, r.manifest_sha256, r.min_client_version, r.release_id
		   FROM release r
		   JOIN channel_head h ON h.release_id = r.release_id
		  ORDER BY r.channel`)
	if err != nil {
		t.Fatalf("read releases: %v", err)
	}
	defer rows.Close()

	seen := 0
	for rows.Next() {
		seen++
		var (
			channel  string
			manifest []byte
			sum      string
			minVer   string
			id       int64
		)
		if err := rows.Scan(&channel, &manifest, &sum, &minVer, &id); err != nil {
			t.Fatalf("scan: %v", err)
		}

		var doc map[string]any
		if err := json.Unmarshal(manifest, &doc); err != nil {
			t.Fatalf("%s: manifest is not JSON: %v", channel, err)
		}

		canonical, err := json.Marshal(doc)
		if err != nil {
			t.Fatalf("%s: re-marshal: %v", channel, err)
		}
		got := sha256.Sum256(canonical)
		if hex.EncodeToString(got[:]) != sum {
			t.Errorf("%s: manifest_sha256 = %s, but the canonical manifest hashes to %s\nmanifest: %s",
				channel, sum, hex.EncodeToString(got[:]), canonical)
		}

		// The manifest has to agree with its own row, since the client reads the
		// manifest and Patch's access log reads the column.
		if doc["channel"] != channel {
			t.Errorf("%s: manifest channel = %v", channel, doc["channel"])
		}
		if doc["min_client_version"] != minVer {
			t.Errorf("%s: manifest min_client_version = %v, column = %q", channel, doc["min_client_version"], minVer)
		}
		if idFloat, ok := doc["release_id"].(float64); !ok || int64(idFloat) != id {
			t.Errorf("%s: manifest release_id = %v, column = %d", channel, doc["release_id"], id)
		}
		if format, ok := doc["format"].(float64); !ok || int(format) != 1 {
			t.Errorf("%s: manifest format = %v, want 1", channel, doc["format"])
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if seen != 3 {
		t.Errorf("checked %d channels, want 3", seen)
	}
}

// TestServerManifestColumnsMustPair covers migration 00003's CHECK: a release row may
// carry both server_manifest and server_manifest_sha256, or neither (a release written
// before server manifests existed), but not one without the other.
func TestServerManifestColumnsMustPair(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	body := []byte(`{"channel":"dev","config":{},"format":1,"min_client_version":"0.0.0","packs":[],"release_id":0}`)
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])

	nextID := func(t *testing.T) int64 {
		t.Helper()
		var id int64
		if err := db.Pool.QueryRow(ctx,
			`SELECT nextval(pg_get_serial_sequence('release','release_id'))`).Scan(&id); err != nil {
			t.Fatalf("next release id: %v", err)
		}
		return id
	}

	// A server manifest without its hash is rejected.
	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO release (release_id, channel, manifest, manifest_sha256,
		                      server_manifest, min_client_version, message, created_by)
		 VALUES ($1, 'dev', $2, $3, $2, '0.0.0', 'unpaired', 'test')`,
		nextID(t), body, sha); err == nil {
		t.Error("INSERT with only server_manifest was accepted, want the CHECK to reject it")
	}
	// A server hash without its manifest is rejected.
	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO release (release_id, channel, manifest, manifest_sha256,
		                      server_manifest_sha256, min_client_version, message, created_by)
		 VALUES ($1, 'dev', $2, $3, $4, '0.0.0', 'unpaired', 'test')`,
		nextID(t), body, sha, sha); err == nil {
		t.Error("INSERT with only server_manifest_sha256 was accepted, want the CHECK to reject it")
	}
}

func TestWriteAuditRequiresEveryField(t *testing.T) {
	// No database needed: these are rejected before the insert is attempted, which is
	// the point — a caller must not be able to write a row nobody can attribute.
	full := store.Entry{ActorID: "1", ActorName: "Someone", Action: "release.publish", Target: "live"}

	tests := []struct {
		name  string
		entry store.Entry
	}{
		{"no actor id", store.Entry{ActorName: full.ActorName, Action: full.Action, Target: full.Target}},
		{"no actor name", store.Entry{ActorID: full.ActorID, Action: full.Action, Target: full.Target}},
		{"no action", store.Entry{ActorID: full.ActorID, ActorName: full.ActorName, Target: full.Target}},
		{"no target", store.Entry{ActorID: full.ActorID, ActorName: full.ActorName, Action: full.Action}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := store.WriteAudit(context.Background(), nil, tt.entry); err == nil {
				t.Fatal("WriteAudit accepted an incomplete entry")
			}
		})
	}
}

// TestAuditLandsInTheCallersTransaction is CFG-A4, and it is the reason WriteAudit
// takes a pgx.Tx rather than the pool: an audit row that survives the change it
// describes being rolled back is worse than a missing one, because a reader cannot
// tell it apart from a truthful record.
func TestAuditLandsInTheCallersTransaction(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	entry := store.Entry{
		ActorID:   "staff-" + uuid.New().String(),
		ActorName: "Test Operator",
		Action:    "release.publish",
		Target:    "live",
		Details:   map[string]any{"release_id": float64(42), "packs": []any{"a", "b"}},
	}

	t.Run("rolled back", func(t *testing.T) {
		tx, err := db.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := store.WriteAudit(ctx, tx, entry); err != nil {
			t.Fatalf("WriteAudit: %v", err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback: %v", err)
		}

		if n := countAudit(t, db, ctx, entry.ActorID); n != 0 {
			t.Errorf("%d audit rows survived a rollback, want 0", n)
		}
	})

	t.Run("committed", func(t *testing.T) {
		tx, err := db.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := store.WriteAudit(ctx, tx, entry); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("WriteAudit: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}

		var (
			actor, name, action, target string
			details                     []byte
		)
		if err := db.Pool.QueryRow(ctx,
			`SELECT actor_id, actor_name, action, target, details FROM audit_log
			  WHERE actor_id = $1`, entry.ActorID).Scan(&actor, &name, &action, &target, &details); err != nil {
			t.Fatalf("read the audit row: %v", err)
		}
		if actor != entry.ActorID || name != entry.ActorName || action != entry.Action || target != entry.Target {
			t.Errorf("row = %q/%q/%q/%q, want %q/%q/%q/%q",
				actor, name, action, target, entry.ActorID, entry.ActorName, entry.Action, entry.Target)
		}

		var got map[string]any
		if err := json.Unmarshal(details, &got); err != nil {
			t.Fatalf("details are not JSON: %v", err)
		}
		if got["release_id"] != float64(42) {
			t.Errorf("details = %v, want release_id 42", got)
		}
	})
}

// TestEmptyAuditDetailsBecomeAnObject covers the column's NOT NULL DEFAULT '{}': a nil
// map must not reach Postgres as SQL NULL.
func TestEmptyAuditDetailsBecomeAnObject(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	entry := store.Entry{
		ActorID:   "staff-" + uuid.New().String(),
		ActorName: "Test Operator",
		Action:    "draft.save",
		Target:    "gameplay",
	}

	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := store.WriteAudit(ctx, tx, entry); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("WriteAudit: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var details []byte
	if err := db.Pool.QueryRow(ctx,
		`SELECT details FROM audit_log WHERE actor_id = $1`, entry.ActorID).Scan(&details); err != nil {
		t.Fatalf("read details: %v", err)
	}
	if string(details) != "{}" {
		t.Errorf("details = %s, want {}", details)
	}
}

func countAudit(t *testing.T, db *store.DB, ctx context.Context, actorID string) int {
	t.Helper()

	var n int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE actor_id = $1`, actorID).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n
}
