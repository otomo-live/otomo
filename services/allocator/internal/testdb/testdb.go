// Package testdb opens the throwaway Postgres the Allocator's DB-backed tests run
// against. It is imported only by _test.go files.
//
// Tests using it skip unless ALLOCATOR_TEST_DATABASE_URL is set, so a plain
// `go test ./...` stays green without a database; ci/compose.yaml sets it, which is
// where they run for real. Two advisory locks keep package-parallel tests honest: the
// schema is migrated under a short one, and then a session-level lock is held on a
// dedicated connection for the whole test, so two packages never share the tables.
// Tests must not call t.Parallel — the lock is per connection, so a parallel sibling
// would run against the same truncated tables.
package testdb

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/otomo-live/otomo/services/allocator/migrations"
)

// EnvVar names the database the tests use. Point it at a database you do not care
// about: tests create rows, and Open truncates the two registry tables first.
const EnvVar = "ALLOCATOR_TEST_DATABASE_URL"

// migrateLockKey is an arbitrary constant shared by every package's tests. It guards
// only the migration, so two packages starting at once do not race to create goose's
// version table.
const migrateLockKey int64 = 0x616c6c6f // "allo"

// testLockKey is a second, distinct lock taken after migration and held for the whole
// test. It is what stops one package's tests from truncating the tables while another
// package's tests are using them.
const testLockKey int64 = 0x616c6c31 // "all1"

// URL returns the test database URL, skipping t when none is configured.
func URL(t testing.TB) string {
	t.Helper()
	url := os.Getenv(EnvVar)
	if url == "" {
		t.Skip(EnvVar + " not set")
	}
	return url
}

// Open returns a pool on the migrated, truncated test database, closed when t ends.
//
// The lock is taken on a connection checked out of the pool and released in t.Cleanup
// after the unlock, so it is held for exactly as long as the test's rows exist.
func Open(t testing.TB) *pgxpool.Pool {
	t.Helper()
	url := URL(t)
	Migrate(t, url)

	ctx := context.Background()
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}

	conn, err := db.Acquire(ctx)
	if err != nil {
		db.Close()
		t.Fatalf("acquire lock connection: %v", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, testLockKey); err != nil {
		conn.Release()
		db.Close()
		t.Fatalf("take test lock: %v", err)
	}
	t.Cleanup(func() {
		// Unlock before returning the connection to the pool: a session-level lock
		// outlives the statement that took it, so a connection reused without
		// unlocking would hold it forever.
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, testLockKey)
		conn.Release()
		db.Close()
	})

	// Both tables are named in one TRUNCATE because allocation has a foreign key to
	// game_server; Postgres refuses to truncate a referenced table otherwise.
	if _, err := db.Exec(ctx, `TRUNCATE allocation, game_server`); err != nil {
		t.Fatalf("truncate registry tables: %v", err)
	}
	return db
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
