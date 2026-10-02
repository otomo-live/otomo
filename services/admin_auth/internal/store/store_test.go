package store_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/otomo-live/otomo/services/admin_auth/internal/config"
	"github.com/otomo-live/otomo/services/admin_auth/internal/store"
	"github.com/otomo-live/otomo/services/admin_auth/migrations"
)

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

// testDBLockKey serialises every DB-backed test package against the shared throwaway
// database. package test binaries run in parallel, and TestMigrateUpDownUp drops the
// staff tables, so this advisory lock is what stops another package's test from
// running while the schema is being rebuilt.
const testDBLockKey int64 = 0x41444D494E // "ADMIN"

func testDB(t *testing.T) (*store.DB, context.Context) {
	t.Helper()

	url := os.Getenv("ADMIN_AUTH_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ADMIN_AUTH_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()

	lockDB, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	conn, err := lockDB.Conn(ctx)
	if err != nil {
		_ = lockDB.Close()
		t.Fatalf("sql.Conn: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, testDBLockKey); err != nil {
		_ = conn.Close()
		_ = lockDB.Close()
		t.Fatalf("acquire the test database lock: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, testDBLockKey)
		_ = conn.Close()
		_ = lockDB.Close()
	})

	db, err := store.NewPool(ctx, config.Config{DatabaseURL: url, DBMaxConns: 4})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(db.Close)

	return db, ctx
}

// uniqueEmail returns an address the shared test database has almost certainly never
// seen. Suffixing the nanosecond clock keeps concurrent and repeat runs apart.
func uniqueEmail(prefix string) string {
	return fmt.Sprintf("%s-%d@example.com", prefix, time.Now().UnixNano())
}

// pgCode runs a statement that is expected to fail and returns its Postgres error
// code, failing the test if it succeeds or fails for a non-Postgres reason.
func pgCode(t *testing.T, ctx context.Context, db *store.DB, query string, args ...any) string {
	t.Helper()

	_, err := db.Pool.Exec(ctx, query, args...)
	if err == nil {
		t.Fatalf("statement succeeded, want an error: %s", query)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error is %T, want *pgconn.PgError: %v", err, err)
	}
	return pgErr.Code
}

// createStaff inserts a staff row and returns its id as text. password_hash is a
// placeholder: these tests exercise the constraints, not argon2id.
func createStaff(t *testing.T, ctx context.Context, db *store.DB, email string, roles []string, root bool) string {
	t.Helper()

	var id string
	if err := db.Pool.QueryRow(ctx,
		`INSERT INTO staff_user (email, name, password_hash, roles, is_root)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id::text`,
		email, "Test Staff", "$argon2id$v=19$m=65536,t=3,p=4$placeholder", roles, root,
	).Scan(&id); err != nil {
		t.Fatalf("insert staff_user: %v", err)
	}
	return id
}

// TestMigrateIsIdempotent applies the embedded migrations twice and checks goose
// recorded the single version exactly once.
func TestMigrateIsIdempotent(t *testing.T) {
	db, ctx := testDB(t)
	url := os.Getenv("ADMIN_AUTH_TEST_DATABASE_URL")

	applyMigrations(t, url)
	applyMigrations(t, url)

	var applied int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM goose_db_version WHERE version_id > 0 AND is_applied`).Scan(&applied); err != nil {
		t.Fatalf("count applied migrations: %v", err)
	}
	// Every embedded migration is recorded exactly once, however many times Up runs.
	files, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatalf("list embedded migrations: %v", err)
	}
	if applied != len(files) {
		t.Errorf("goose recorded %d applied migrations, want %d (one per embedded file)", applied, len(files))
	}
}

// TestMigrateUpDownUp round-trips the whole schema and leaves it Up, so the Down side
// is proven to drop every object the Up side creates.
//
// It first resets the public schema because the shared test database may have recorded
// migration version 1 back when it was the AA-1 no-op, in which case goose believes the
// schema is current while no objects exist.
func TestMigrateUpDownUp(t *testing.T) {
	db, ctx := testDB(t)
	url := os.Getenv("ADMIN_AUTH_TEST_DATABASE_URL")

	sqlDB, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer sqlDB.Close()

	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("SetDialect: %v", err)
	}
	goose.SetBaseFS(migrations.FS)

	if err := goose.Up(sqlDB, "."); err != nil {
		t.Fatalf("goose.Up before Down: %v", err)
	}
	if err := goose.DownTo(sqlDB, ".", 0); err != nil {
		t.Fatalf("goose.DownTo(0): %v", err)
	}
	if err := goose.Up(sqlDB, "."); err != nil {
		t.Fatalf("goose.Up after Down: %v", err)
	}

	// The schema must be usable after the round trip.
	id := createStaff(t, ctx, db, uniqueEmail("roundtrip"), []string{"viewer"}, false)
	if _, err := db.Pool.Exec(ctx, `DELETE FROM staff_user WHERE id = $1::uuid`, id); err != nil {
		t.Fatalf("cleanup after round trip: %v", err)
	}
}

func TestEmailUniquenessIsCaseInsensitive(t *testing.T) {
	db, ctx := testDB(t)
	applyMigrations(t, os.Getenv("ADMIN_AUTH_TEST_DATABASE_URL"))

	email := uniqueEmail("Case") // mixed-case local part
	createStaff(t, ctx, db, email, []string{"viewer"}, false)
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM staff_user WHERE lower(email) = lower($1)`, email)
	})

	// The uppercase and lowercase spellings must collide on the lower(email) index.
	code := pgCode(t, ctx, db,
		`INSERT INTO staff_user (email, name, password_hash, roles) VALUES ($1, $2, $3, $4)`,
		strings.ToLower(email), "Test Staff", "x", []string{"viewer"})
	if code != "23505" {
		t.Errorf("case-insensitive duplicate error code = %s, want 23505", code)
	}
}

func TestRolesCheck(t *testing.T) {
	db, ctx := testDB(t)
	applyMigrations(t, os.Getenv("ADMIN_AUTH_TEST_DATABASE_URL"))

	email := uniqueEmail("Roles")
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM staff_user WHERE lower(email) = lower($1)`, email)
	})

	if code := pgCode(t, ctx, db,
		`INSERT INTO staff_user (email, name, password_hash, roles) VALUES ($1, $2, $3, $4)`,
		uniqueEmail("Empty"), "Test Staff", "x", []string{}); code != "23514" {
		t.Errorf("empty roles error code = %s, want 23514", code)
	}
	if code := pgCode(t, ctx, db,
		`INSERT INTO staff_user (email, name, password_hash, roles) VALUES ($1, $2, $3, $4)`,
		uniqueEmail("Super"), "Test Staff", "x", []string{"superuser"}); code != "23514" {
		t.Errorf("bad role error code = %s, want 23514", code)
	}

	createStaff(t, ctx, db, email, []string{"viewer", "admin"}, false)
}

func TestSingleRoot(t *testing.T) {
	db, ctx := testDB(t)
	applyMigrations(t, os.Getenv("ADMIN_AUTH_TEST_DATABASE_URL"))

	var existing int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM staff_user WHERE is_root`).Scan(&existing); err != nil {
		t.Fatalf("count roots: %v", err)
	}
	if existing > 0 {
		t.Skip("a root staff row already exists")
	}

	id := createStaff(t, ctx, db, uniqueEmail("root"), []string{"admin"}, true)
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM staff_user WHERE id = $1::uuid`, id)
	})

	code := pgCode(t, ctx, db,
		`INSERT INTO staff_user (email, name, password_hash, roles, is_root)
		 VALUES ($1, $2, $3, $4, true)`,
		uniqueEmail("root2"), "Test Staff", "x", []string{"admin"})
	if code != "23505" {
		t.Errorf("second root error code = %s, want 23505", code)
	}
}

func TestInviteChecks(t *testing.T) {
	db, ctx := testDB(t)
	applyMigrations(t, os.Getenv("ADMIN_AUTH_TEST_DATABASE_URL"))

	creator := createStaff(t, ctx, db, uniqueEmail("InviteCreator"), []string{"admin"}, false)
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM staff_user WHERE id = $1::uuid`, creator)
	})

	token := func() []byte { return []byte(uniqueEmail("token")) }

	// invite without a role.
	if code := pgCode(t, ctx, db,
		`INSERT INTO staff_invite (token_hash, purpose, email, name, created_by, expires_at)
		 VALUES ($1, 'invite', $2, $3, $4::uuid, now() + interval '1 hour')`,
		token(), uniqueEmail("invitee"), "Invitee", creator); code != "23514" {
		t.Errorf("invite without role error code = %s, want 23514", code)
	}

	// reset without a user_id.
	if code := pgCode(t, ctx, db,
		`INSERT INTO staff_invite (token_hash, purpose, email, name, created_by, expires_at)
		 VALUES ($1, 'reset', $2, $3, $4::uuid, now() + interval '1 hour')`,
		token(), uniqueEmail("invitee"), "Invitee", creator); code != "23514" {
		t.Errorf("reset without user_id error code = %s, want 23514", code)
	}

	// expires_at not after created_at.
	if code := pgCode(t, ctx, db,
		`INSERT INTO staff_invite (token_hash, purpose, email, name, role, created_by, expires_at)
		 VALUES ($1, 'invite', $2, $3, 'viewer', $4::uuid, now() - interval '1 hour')`,
		token(), uniqueEmail("invitee"), "Invitee", creator); code != "23514" {
		t.Errorf("expired invite error code = %s, want 23514", code)
	}
}

// randomPublicKey returns 32 fresh bytes, so a key inserted by one run can never be
// confused with a key left behind by an earlier run against the same shared database.
func randomPublicKey(t *testing.T) []byte {
	t.Helper()
	pub := make([]byte, 32)
	if _, err := rand.Read(pub); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return pub
}

// signingKeyFixture inserts a genkey-style key row and returns its kid and public
// bytes. Kids are unique per run so repeat runs against a shared database do not
// collide on the primary key.
func signingKeyFixture(t *testing.T, ctx context.Context, db *store.DB) (string, []byte) {
	t.Helper()

	applyMigrations(t, os.Getenv("ADMIN_AUTH_TEST_DATABASE_URL"))

	kid := "test-" + uuid.New().String()
	pub := randomPublicKey(t)
	if err := db.InsertSigningKey(ctx, kid, pub); err != nil {
		t.Fatalf("InsertSigningKey: %v", err)
	}
	return kid, pub
}

func TestSigningKeyRoundTrip(t *testing.T) {
	db, ctx := testDB(t)

	kid, pub := signingKeyFixture(t, ctx, db)

	got, err := db.FindActiveByPublicKey(ctx, pub)
	if err != nil {
		t.Fatalf("FindActiveByPublicKey: %v", err)
	}
	if got != kid {
		t.Errorf("kid = %q, want %q", got, kid)
	}

	keys, err := db.ListActiveSigningKeys(ctx)
	if err != nil {
		t.Fatalf("ListActiveSigningKeys: %v", err)
	}
	found := false
	for _, k := range keys {
		if k.Kid == kid {
			found = true
			if !bytes.Equal(k.PublicKey, pub) {
				t.Errorf("public key for %s round-tripped as %x", kid, k.PublicKey)
			}
		}
	}
	if !found {
		t.Errorf("%s is missing from ListActiveSigningKeys", kid)
	}
}

func TestInsertSigningKeyRejectsADuplicateKid(t *testing.T) {
	db, ctx := testDB(t)

	kid, pub := signingKeyFixture(t, ctx, db)

	if err := db.InsertSigningKey(ctx, kid, pub); err == nil {
		t.Fatal("a duplicate kid was accepted")
	}
}

func TestFindActiveByPublicKeyReportsNotFound(t *testing.T) {
	db, ctx := testDB(t)

	applyMigrations(t, os.Getenv("ADMIN_AUTH_TEST_DATABASE_URL"))

	_, err := db.FindActiveByPublicKey(ctx, randomPublicKey(t))
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want store.ErrNotFound", err)
	}
}

func TestStaffDeleteCascades(t *testing.T) {
	db, ctx := testDB(t)
	applyMigrations(t, os.Getenv("ADMIN_AUTH_TEST_DATABASE_URL"))

	id := createStaff(t, ctx, db, uniqueEmail("Cascade"), []string{"viewer"}, false)

	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO refresh_session (family_id, user_id, token_hash, expires_at)
		 VALUES (gen_random_uuid(), $1::uuid, $2, now() + interval '1 hour')`,
		id, []byte(uniqueEmail("session"))); err != nil {
		t.Fatalf("insert refresh_session: %v", err)
	}
	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO mfa_recovery_code (user_id, code_hash) VALUES ($1::uuid, $2)`,
		id, []byte(uniqueEmail("code"))); err != nil {
		t.Fatalf("insert mfa_recovery_code: %v", err)
	}

	if _, err := db.Pool.Exec(ctx, `DELETE FROM staff_user WHERE id = $1::uuid`, id); err != nil {
		t.Fatalf("delete staff_user: %v", err)
	}

	for _, table := range []string{"refresh_session", "mfa_recovery_code"} {
		var n int
		if err := db.Pool.QueryRow(ctx,
			`SELECT count(*) FROM `+table+` WHERE user_id = $1::uuid`, id).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s still has %d rows after staff_user delete", table, n)
		}
	}
}
