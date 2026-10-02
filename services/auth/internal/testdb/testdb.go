// Package testdb opens the throwaway Postgres the DB-backed tests run against. It is
// imported only by _test.go files.
//
// Tests using it skip unless AUTH_TEST_DATABASE_URL is set, so a plain `go test ./...`
// stays green without a database; ci/compose.yaml sets it, which is where they run for
// real. The schema is migrated under a Postgres advisory lock because `go test ./...`
// runs packages in parallel, and two packages migrating one fresh database at once
// would race to create goose's version table.
package testdb

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/otomo-live/otomo/services/auth/internal/config"
	"github.com/otomo-live/otomo/services/auth/internal/store"
	"github.com/otomo-live/otomo/services/auth/migrations"
)

// EnvVar names the database the tests use. Point it at a database you do not care
// about: tests create rows and never delete them.
const EnvVar = "AUTH_TEST_DATABASE_URL"

// migrateLockKey is an arbitrary constant shared by every package's tests.
const migrateLockKey = 0x61757468 // "auth"

// URL returns the test database URL, skipping t when none is configured.
func URL(t testing.TB) string {
	t.Helper()
	url := os.Getenv(EnvVar)
	if url == "" {
		t.Skip(EnvVar + " not set")
	}
	return url
}

// Open returns a pool on the migrated test database, closed when t ends.
func Open(t testing.TB) (*store.DB, context.Context) {
	t.Helper()
	url := URL(t)
	Migrate(t, url)

	ctx := context.Background()
	db, err := store.NewPool(ctx, config.Config{DatabaseURL: url, DBMaxConns: 4})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(db.Close)
	return db, ctx
}

// Migrate applies the embedded migrations to url, holding a session-level advisory
// lock for the duration so concurrent test packages take turns.
func Migrate(t testing.TB, url string) {
	t.Helper()
	sqlDB, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer sqlDB.Close()

	ctx := context.Background()
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrateLockKey); err != nil {
		t.Fatalf("take migration lock: %v", err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, migrateLockKey) }()

	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("SetDialect: %v", err)
	}
	if err := goose.Up(sqlDB, "."); err != nil {
		t.Fatalf("goose.Up: %v", err)
	}
}
