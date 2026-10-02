package bootstrap_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/otomo-live/otomo/services/admin_auth/internal/bootstrap"
	"github.com/otomo-live/otomo/services/admin_auth/internal/config"
	"github.com/otomo-live/otomo/services/admin_auth/internal/password"
	"github.com/otomo-live/otomo/services/admin_auth/internal/server"
	"github.com/otomo-live/otomo/services/admin_auth/internal/store"
	"github.com/otomo-live/otomo/services/admin_auth/internal/token"
	"github.com/otomo-live/otomo/services/admin_auth/migrations"
)

// testDBLockKey serialises every DB-backed test package against the shared throwaway
// database. package test binaries run in parallel, and internal/store's migration
// round-trip drops the staff tables, so a cross-package advisory lock is what keeps
// the bootstrap tests from running while the schema is being rebuilt.
const testDBLockKey int64 = 0x41444D494E // "ADMIN"

func testDB(t *testing.T) (*store.DB, context.Context) {
	t.Helper()

	url := os.Getenv("ADMIN_AUTH_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ADMIN_AUTH_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()

	sqlDB, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		_ = sqlDB.Close()
		t.Fatalf("sql.Conn: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, testDBLockKey); err != nil {
		_ = conn.Close()
		_ = sqlDB.Close()
		t.Fatalf("acquire the test database lock: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, testDBLockKey)
		_ = conn.Close()
		_ = sqlDB.Close()
	})

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("SetDialect: %v", err)
	}
	if err := goose.Up(sqlDB, "."); err != nil {
		t.Fatalf("goose.Up: %v", err)
	}

	db, err := store.NewPool(ctx, config.Config{DatabaseURL: url, DBMaxConns: 8})
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(db.Close)

	return db, ctx
}

// cleanRoot removes any root the shared test database may hold and registers a
// cleanup that removes the root the test creates. Dependent rows (refresh_session,
// staff_invite, mfa_recovery_code) cascade from staff_user.
func cleanRoot(t *testing.T, db *store.DB) {
	t.Helper()

	if _, err := db.Pool.Exec(context.Background(), `DELETE FROM staff_user WHERE is_root`); err != nil {
		t.Fatalf("delete existing root: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM staff_user WHERE is_root`)
	})
}

func uniqueEmail(prefix string) string {
	return fmt.Sprintf("%s-%d@example.com", prefix, time.Now().UnixNano())
}

func writePasswordFile(t *testing.T, content string, perm os.FileMode) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "root_password")
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatalf("write password file: %v", err)
	}
	// WriteFile only applies perm on creation; chmod so a restrictive umask cannot
	// hide a test that means to exercise the loose-permission warning.
	if err := os.Chmod(path, perm); err != nil {
		t.Fatalf("chmod password file: %v", err)
	}
	return path
}

func rootRow(t *testing.T, ctx context.Context, db *store.DB) (id, email, hash string, roles []string, isRoot bool, createdByNull bool) {
	t.Helper()

	err := db.Pool.QueryRow(ctx,
		`SELECT id::text, email, password_hash, roles, is_root, created_by IS NULL
		 FROM staff_user WHERE is_root`,
	).Scan(&id, &email, &hash, &roles, &isRoot, &createdByNull)
	if err != nil {
		t.Fatalf("read root row: %v", err)
	}
	return id, email, hash, roles, isRoot, createdByNull
}

func TestRunCreatesRoot(t *testing.T) {
	db, ctx := testDB(t)
	cleanRoot(t, db)

	const pw = "correct horse battery staple"
	path := writePasswordFile(t, pw+"\n", 0o600)
	email := uniqueEmail("RootCreate")

	var warn bytes.Buffer
	msg, err := bootstrap.Run(ctx, db, bootstrap.Options{
		Email: email, Name: "Break Glass", PasswordFile: path, Warn: &warn,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := "root created: " + strings.ToLower(email); msg != want {
		t.Errorf("msg = %q, want %q", msg, want)
	}
	if warn.Len() != 0 {
		t.Errorf("unexpected warning for a 0600 file: %q", warn.String())
	}

	id, gotEmail, hash, roles, isRoot, createdByNull := rootRow(t, ctx, db)
	if gotEmail != strings.ToLower(email) {
		t.Errorf("email = %q, want the lowercased %q", gotEmail, strings.ToLower(email))
	}
	if len(roles) != 1 || roles[0] != "admin" {
		t.Errorf("roles = %v, want [admin]", roles)
	}
	if !isRoot {
		t.Error("is_root = false, want true")
	}
	if !createdByNull {
		t.Error("created_by is not NULL for the root row")
	}

	ok, err := password.Verify(hash, pw)
	if err != nil {
		t.Fatalf("password.Verify: %v", err)
	}
	if !ok {
		t.Error("the stored hash does not verify the file's password")
	}

	var audits int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE action = 'root.bootstrap' AND target = $1`, id).Scan(&audits); err != nil {
		t.Fatalf("count root.bootstrap audit: %v", err)
	}
	if audits != 1 {
		t.Errorf("root.bootstrap audit rows = %d, want 1", audits)
	}

	var actorID, actorName string
	if err := db.Pool.QueryRow(ctx,
		`SELECT actor_id, actor_name FROM audit_log WHERE action = 'root.bootstrap' AND target = $1`, id).Scan(&actorID, &actorName); err != nil {
		t.Fatalf("read root.bootstrap actor: %v", err)
	}
	if actorID != "bootstrap" || actorName != "bootstrap" {
		t.Errorf("actor = %q/%q, want bootstrap/bootstrap", actorID, actorName)
	}
}

func TestRunIsIdempotent(t *testing.T) {
	db, ctx := testDB(t)
	cleanRoot(t, db)

	path := writePasswordFile(t, "first-password-long-enough\n", 0o600)
	email := uniqueEmail("RootIdem")
	if _, err := bootstrap.Run(ctx, db, bootstrap.Options{Email: email, Name: "Root", PasswordFile: path}); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	id, _, hashBefore, _, _, _ := rootRow(t, ctx, db)

	msg, err := bootstrap.Run(ctx, db, bootstrap.Options{
		Email: uniqueEmail("DifferentRoot"), Name: "Other", PasswordFile: path,
	})
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if !strings.Contains(msg, "root already exists: "+strings.ToLower(email)) {
		t.Errorf("msg = %q, want the existing email", msg)
	}
	if !strings.Contains(msg, "note:") || !strings.Contains(msg, strings.ToLower(email)) {
		t.Errorf("msg = %q, want a note that the existing email is kept", msg)
	}

	_, _, hashAfter, _, _, _ := rootRow(t, ctx, db)
	if hashAfter != hashBefore {
		t.Error("a second run without -rotate changed the stored hash")
	}

	var audits int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE action = 'root.bootstrap' AND target = $1`, id).Scan(&audits); err != nil {
		t.Fatalf("count root.bootstrap audit: %v", err)
	}
	if audits != 1 {
		t.Errorf("root.bootstrap audit rows after a no-op = %d, want 1", audits)
	}
}

func TestRunRotatesRoot(t *testing.T) {
	db, ctx := testDB(t)
	cleanRoot(t, db)

	const first, second = "first-password-long-enough", "second-password-long-enough"
	path := writePasswordFile(t, first+"\n", 0o600)
	email := uniqueEmail("RootRotate")
	if _, err := bootstrap.Run(ctx, db, bootstrap.Options{Email: email, Name: "Root", PasswordFile: path}); err != nil {
		t.Fatalf("create Run: %v", err)
	}

	id, _, hashBefore, _, _, _ := rootRow(t, ctx, db)
	var changedBefore time.Time
	if err := db.Pool.QueryRow(ctx, `SELECT password_changed_at FROM staff_user WHERE id = $1::uuid`, id).Scan(&changedBefore); err != nil {
		t.Fatalf("read password_changed_at: %v", err)
	}

	for i := 0; i < 2; i++ {
		tokenHash := make([]byte, 16)
		if _, err := rand.Read(tokenHash); err != nil {
			t.Fatalf("rand.Read: %v", err)
		}
		if _, err := db.Pool.Exec(ctx,
			`INSERT INTO refresh_session (family_id, user_id, token_hash, expires_at)
			 VALUES (gen_random_uuid(), $1::uuid, $2, now() + interval '1 hour')`,
			id, tokenHash); err != nil {
			t.Fatalf("insert refresh_session: %v", err)
		}
	}
	if _, err := db.Pool.Exec(ctx,
		`UPDATE staff_user SET failed_logins = 3, locked_until = now() + interval '1 hour' WHERE id = $1::uuid`, id); err != nil {
		t.Fatalf("seed lockout: %v", err)
	}

	time.Sleep(2 * time.Millisecond)
	rotatePath := writePasswordFile(t, second+"\n", 0o600)
	msg, err := bootstrap.Run(ctx, db, bootstrap.Options{Email: email, Name: "Root", PasswordFile: rotatePath, Rotate: true})
	if err != nil {
		t.Fatalf("rotate Run: %v", err)
	}
	if want := "root password rotated; 2 sessions revoked"; msg != want {
		t.Errorf("msg = %q, want %q", msg, want)
	}

	_, _, hashAfter, _, _, _ := rootRow(t, ctx, db)
	if hashAfter == hashBefore {
		t.Error("the stored hash did not change")
	}
	if ok, err := password.Verify(hashAfter, first); err != nil || ok {
		t.Errorf("old password verify = %v (err %v), want false/nil", ok, err)
	}
	if ok, err := password.Verify(hashAfter, second); err != nil || !ok {
		t.Errorf("new password verify = %v (err %v), want true/nil", ok, err)
	}

	var failed int
	var lockedNull bool
	var changedAfter time.Time
	if err := db.Pool.QueryRow(ctx,
		`SELECT failed_logins, locked_until IS NULL, password_changed_at FROM staff_user WHERE id = $1::uuid`, id).
		Scan(&failed, &lockedNull, &changedAfter); err != nil {
		t.Fatalf("read root state after rotate: %v", err)
	}
	if failed != 0 {
		t.Errorf("failed_logins = %d, want 0", failed)
	}
	if !lockedNull {
		t.Error("locked_until is not NULL after rotate")
	}
	if !changedAfter.After(changedBefore) {
		t.Errorf("password_changed_at = %s, want after %s", changedAfter, changedBefore)
	}

	var revoked int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM refresh_session WHERE user_id = $1::uuid AND revoked_at IS NOT NULL`, id).Scan(&revoked); err != nil {
		t.Fatalf("count revoked sessions: %v", err)
	}
	if revoked != 2 {
		t.Errorf("revoked sessions = %d, want 2", revoked)
	}

	var audits int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE action = 'root.rotate' AND target = $1`, id).Scan(&audits); err != nil {
		t.Fatalf("count root.rotate audit: %v", err)
	}
	if audits != 1 {
		t.Errorf("root.rotate audit rows = %d, want 1", audits)
	}
}

func TestRunRotateWithNoRootCreates(t *testing.T) {
	db, ctx := testDB(t)
	cleanRoot(t, db)

	path := writePasswordFile(t, "rotate-but-none-exists\n", 0o600)
	msg, err := bootstrap.Run(ctx, db, bootstrap.Options{Email: uniqueEmail("RootRotateNew"), Name: "Root", PasswordFile: path, Rotate: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(msg, "root created") || !strings.Contains(msg, "no root existed to rotate") {
		t.Errorf("msg = %q, want a create message noting there was no root to rotate", msg)
	}
}

func TestReadPasswordFileRules(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"empty", ""},
		{"only a newline", "\n"},
		{"fifteen characters", strings.Repeat("a", 15)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writePasswordFile(t, c.content, 0o600)
			_, _, err := bootstrap.ReadPassword(path)
			if err == nil {
				t.Fatal("ReadPassword accepted an invalid file")
			}
			if c.content != "" && strings.Contains(err.Error(), c.content) {
				t.Errorf("error %q contains the file contents", err)
			}
		})
	}

	t.Run("missing", func(t *testing.T) {
		if _, _, err := bootstrap.ReadPassword(filepath.Join(t.TempDir(), "nope")); err == nil {
			t.Fatal("ReadPassword accepted a missing file")
		}
	})

	t.Run("trims exactly one line ending", func(t *testing.T) {
		path := writePasswordFile(t, "the password is long\n", 0o600)
		pw, _, err := bootstrap.ReadPassword(path)
		if err != nil {
			t.Fatalf("ReadPassword: %v", err)
		}
		if pw != "the password is long" {
			t.Errorf("pw = %q, want the trailing newline trimmed", pw)
		}

		crlf := writePasswordFile(t, "the password is long\r\n", 0o600)
		pw, _, err = bootstrap.ReadPassword(crlf)
		if err != nil {
			t.Fatalf("ReadPassword: %v", err)
		}
		if pw != "the password is long" {
			t.Errorf("pw = %q, want one CRLF trimmed", pw)
		}
	})

	t.Run("warns on a loose file", func(t *testing.T) {
		db, ctx := testDB(t)
		cleanRoot(t, db)

		path := writePasswordFile(t, "loose-permission-password\n", 0o644)
		var warn bytes.Buffer
		if _, err := bootstrap.Run(ctx, db, bootstrap.Options{Email: uniqueEmail("RootLoose"), Name: "Root", PasswordFile: path, Warn: &warn}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !strings.Contains(warn.String(), "world-readable") && !strings.Contains(warn.String(), "group-") {
			t.Errorf("warning = %q, want a permissions warning", warn.String())
		}
	})
}

func TestRunConcurrentBootstrap(t *testing.T) {
	db, ctx := testDB(t)
	cleanRoot(t, db)

	path := writePasswordFile(t, "concurrent-bootstrap-password\n", 0o600)
	email := uniqueEmail("RootConcurrent")

	const workers = 4
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		msgs  []string
		errs  []error
		start = make(chan struct{})
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			msg, err := bootstrap.Run(ctx, db, bootstrap.Options{Email: email, Name: "Root", PasswordFile: path})
			mu.Lock()
			msgs = append(msgs, msg)
			errs = append(errs, err)
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("worker %d: %v", i, err)
		}
	}
	created, exists := 0, 0
	for _, msg := range msgs {
		switch {
		case strings.Contains(msg, "root created"):
			created++
		case strings.Contains(msg, "root already exists"):
			exists++
		}
	}
	if created != 1 || exists != workers-1 {
		t.Errorf("created/already-exists = %d/%d, want 1/%d (messages %v)", created, exists, workers-1, msgs)
	}

	var roots int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM staff_user WHERE is_root`).Scan(&roots); err != nil {
		t.Fatalf("count roots: %v", err)
	}
	if roots != 1 {
		t.Errorf("root rows = %d, want 1", roots)
	}
}

// staticJWKS is the minimal api.JWKSProvider a test server needs.
type staticJWKS []byte

func (s staticJWKS) Bytes() []byte { return s }

func startLoginServer(t *testing.T, db *store.DB) string {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	signer := &token.Signer{
		Kid:        "admin-auth-test",
		PrivateKey: priv,
		Issuer:     "https://admin-auth.otomo.internal",
		Audience:   "otomo:staff",
		TTL:        15 * time.Minute,
	}
	raw, err := token.BuildJWKS([]token.PublicKey{{Kid: "admin-auth-test", Key: pub}})
	if err != nil {
		t.Fatalf("BuildJWKS: %v", err)
	}

	cfg := config.Config{
		ListenAddr:       "127.0.0.1:0",
		MetricsAddr:      "127.0.0.1:0",
		ReadTimeout:      10 * time.Second,
		WriteTimeout:     30 * time.Second,
		IdleTimeout:      120 * time.Second,
		ShutdownTimeout:  10 * time.Second,
		AccessTokenTTL:   15 * time.Minute,
		RefreshTokenTTL:  168 * time.Hour,
		LoginMaxFailures: 5,
		LoginLockout:     15 * time.Minute,
	}
	srv := server.New(cfg, server.Deps{JWKS: staticJWKS(raw), Signer: signer, Store: db})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("server did not shut down")
		}
	})

	select {
	case <-srv.Started():
	case <-time.After(10 * time.Second):
		t.Fatal("server never started listening")
	}
	return "http://" + srv.PublicAddr().String()
}

func TestBootstrapRootCanLogIn(t *testing.T) {
	db, ctx := testDB(t)
	cleanRoot(t, db)

	const pw = "root-break-glass-password"
	path := writePasswordFile(t, pw+"\n", 0o600)
	email := uniqueEmail("RootLogin")
	if _, err := bootstrap.Run(ctx, db, bootstrap.Options{Email: email, Name: "Break Glass", PasswordFile: path}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	base := startLoginServer(t, db)
	body := fmt.Sprintf(`{"email":%q,"password":%q}`, email, pw)
	req, err := http.NewRequest(http.MethodPost, base+"/admin-auth/login", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /admin-auth/login: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read login response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, raw)
	}

	var got struct {
		AccessToken string `json:"access_token"`
		User        struct {
			Roles []string `json:"roles"`
		} `json:"user"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("login body %q is not JSON: %v", raw, err)
	}
	if got.AccessToken == "" {
		t.Error("access_token is empty")
	}
	if len(got.User.Roles) != 1 || got.User.Roles[0] != "admin" {
		t.Errorf("roles = %v, want [admin]", got.User.Roles)
	}
}
