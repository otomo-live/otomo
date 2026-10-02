package store_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/otomo-live/otomo/services/auth/internal/store"
	"github.com/otomo-live/otomo/services/auth/internal/testdb"
)

func testDB(t *testing.T) (*store.DB, context.Context) {
	t.Helper()
	return testdb.Open(t)
}

func TestMigrateIsIdempotent(t *testing.T) {
	db, _ := testDB(t)

	testdb.Migrate(t, testdb.URL(t))

	var tables int
	row := db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_schema = 'public'
		   AND table_name IN ('account', 'identity_binding', 'signing_key', 'refresh_token')`)
	if err := row.Scan(&tables); err != nil {
		t.Fatalf("count tables: %v", err)
	}
	if tables != 4 {
		t.Errorf("found %d of the 4 AUTH-1 tables", tables)
	}
}

func TestSigningKeyRoundTrip(t *testing.T) {
	db, ctx := testDB(t)

	kid := "test-" + uuid.New().String()
	// A fresh key per run: signing_key has no unique constraint on public_key, so a
	// fixed value would find an earlier run's row when rerun against the same
	// database.
	pub := tokenHash(t)

	if err := db.InsertSigningKey(ctx, kid, pub); err != nil {
		t.Fatalf("InsertSigningKey: %v", err)
	}

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

	kid := "test-" + uuid.New().String()
	pub := tokenHash(t)

	if err := db.InsertSigningKey(ctx, kid, pub); err != nil {
		t.Fatalf("first InsertSigningKey: %v", err)
	}
	if err := db.InsertSigningKey(ctx, kid, pub); err == nil {
		t.Fatal("a duplicate kid was accepted")
	}
}

func TestFindActiveByPublicKeyReportsNotFound(t *testing.T) {
	db, ctx := testDB(t)

	_, err := db.FindActiveByPublicKey(ctx, tokenHash(t))
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want store.ErrNotFound", err)
	}
}

func deviceExternalID(t *testing.T) string {
	t.Helper()
	// Shaped like the real value (hex SHA-256) but random, so reruns against the same
	// database never collide with an earlier run's rows.
	return strings.ReplaceAll(uuid.New().String()+uuid.New().String(), "-", "")
}

func countAccounts(t *testing.T, db *store.DB) int {
	t.Helper()
	var n int
	if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM account`).Scan(&n); err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	return n
}

func TestFindOrCreateAccountIsStablePerBinding(t *testing.T) {
	db, ctx := testDB(t)
	extA, extB := deviceExternalID(t), deviceExternalID(t)

	first, created, err := db.FindOrCreateAccount(ctx, store.MethodDevice, extA)
	if err != nil {
		t.Fatalf("first login: %v", err)
	}
	if !created {
		t.Error("the first login did not report creating the account")
	}
	if _, err := uuid.Parse(first); err != nil {
		t.Fatalf("account id %q is not a UUID: %v", first, err)
	}
	second, created, err := db.FindOrCreateAccount(ctx, store.MethodDevice, extA)
	if err != nil {
		t.Fatalf("second login: %v", err)
	}
	if created {
		t.Error("the second login reported creating an account")
	}
	if first != second {
		t.Errorf("same binding produced two accounts: %s and %s", first, second)
	}

	other, _, err := db.FindOrCreateAccount(ctx, store.MethodDevice, extB)
	if err != nil {
		t.Fatalf("other device: %v", err)
	}
	if other == first {
		t.Errorf("two bindings share the account %s", other)
	}

	// The binding is per method: the same external id under another method is a
	// different identity.
	viaOther, _, err := db.FindOrCreateAccount(ctx, "test-method", extA)
	if err != nil {
		t.Fatalf("other method: %v", err)
	}
	if viaOther == first {
		t.Error("a binding under another method resolved to the device account")
	}
}

func TestFindOrCreateAccountConcurrentFirstLoginsMakeOneAccount(t *testing.T) {
	db, ctx := testDB(t)
	ext := deviceExternalID(t)
	before := countAccounts(t, db)

	const n = 10
	ids := make([]string, n)
	created := make([]bool, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			ids[i], created[i], errs[i] = db.FindOrCreateAccount(ctx, store.MethodDevice, ext)
		})
	}
	close(start)
	wg.Wait()

	creators := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("login %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Errorf("login %d got account %s, login 0 got %s", i, ids[i], ids[0])
		}
		if created[i] {
			creators++
		}
	}
	if creators != 1 {
		t.Errorf("%d logins reported creating the account, want 1", creators)
	}
	if got := countAccounts(t, db) - before; got != 1 {
		t.Errorf("%d concurrent first logins created %d accounts, want 1 (no orphans)", n, got)
	}
}

// The losing side of the race, forced deterministically: another transaction holds an
// uncommitted binding, so FindOrCreateAccount misses the lookup, blocks on the binding
// insert, and must then return the winner's account and leave no orphan behind.
func TestFindOrCreateAccountLosingTheRaceReturnsTheWinner(t *testing.T) {
	db, ctx := testDB(t)
	ext := deviceExternalID(t)
	winner := uuid.New().String()

	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO account (account_id) VALUES ($1)`, winner); err != nil {
		t.Fatalf("insert winner account: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO identity_binding (method, external_id, account_id) VALUES ($1, $2, $3)`,
		store.MethodDevice, ext, winner); err != nil {
		t.Fatalf("insert winner binding: %v", err)
	}
	before := countAccounts(t, db) // the winner's uncommitted row is not visible here

	type result struct {
		id  string
		err error
	}
	done := make(chan result, 1)
	go func() {
		id, created, err := db.FindOrCreateAccount(ctx, store.MethodDevice, ext)
		if created {
			err = errors.New("the losing login reported creating the account")
		}
		done <- result{id, err}
	}()

	// Give the loser time to reach the blocking insert, then let the winner commit.
	select {
	case r := <-done:
		t.Fatalf("FindOrCreateAccount returned %v before the winner committed", r)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit winner: %v", err)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("loser: %v", r.err)
		}
		if r.id != winner {
			t.Errorf("loser got account %s, want the winner's %s", r.id, winner)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the loser never returned after the winner committed")
	}
	if got := countAccounts(t, db) - before; got != 1 {
		t.Errorf("accounts grew by %d, want 1: the loser left an orphan account row", got)
	}
}

// newAccount creates a throwaway account for refresh-token tests.
func newAccount(t *testing.T, db *store.DB) string {
	t.Helper()
	id, _, err := db.FindOrCreateAccount(context.Background(), "test-method", deviceExternalID(t))
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	return id
}

// tokenHash returns a random value shaped like a stored refresh-token hash.
func tokenHash(t *testing.T) []byte {
	t.Helper()
	h := make([]byte, 32)
	if _, err := rand.Read(h); err != nil {
		t.Fatal(err)
	}
	return h
}

func rotate(t *testing.T, db *store.DB, from, to []byte) store.RotateResult {
	t.Helper()
	res, err := db.RotateRefreshToken(context.Background(), store.RotateInput{
		TokenHash: from, NewTokenHash: to, TTL: 720 * time.Hour, Now: time.Now(),
	})
	if err != nil {
		t.Fatalf("RotateRefreshToken: %v", err)
	}
	return res
}

func TestRefreshRotationChainsAndReuseRevokesTheFamily(t *testing.T) {
	db, ctx := testDB(t)
	account := newAccount(t, db)
	first, second, third := tokenHash(t), tokenHash(t), tokenHash(t)

	family, err := db.InsertRefreshToken(ctx, account, first, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("InsertRefreshToken: %v", err)
	}

	res := rotate(t, db, first, second)
	if res.Outcome != store.RotateRotated || res.AccountID != account || res.FamilyID != family {
		t.Fatalf("first rotation = %+v, want rotated for %s in %s", res, account, family)
	}
	if res := rotate(t, db, second, third); res.Outcome != store.RotateRotated {
		t.Fatalf("second rotation = %+v, want rotated", res)
	}

	// Replaying the first, already-exchanged token is reuse...
	if res := rotate(t, db, first, tokenHash(t)); res.Outcome != store.RotateReused || res.FamilyID != family {
		t.Fatalf("replay = %+v, want reused in %s", res, family)
	}
	// ...and it took the live head of the chain down with it.
	if res := rotate(t, db, third, tokenHash(t)); res.Outcome != store.RotateRevoked {
		t.Errorf("live token after reuse = %+v, want revoked", res)
	}

	var live int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM refresh_token WHERE family_id = $1 AND NOT revoked`, family).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Errorf("%d tokens in the family are still live after reuse", live)
	}
}

func TestRefreshRotationSlidesTheExpiry(t *testing.T) {
	db, ctx := testDB(t)
	account := newAccount(t, db)
	old, successor := tokenHash(t), tokenHash(t)

	if _, err := db.InsertRefreshToken(ctx, account, old, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := db.RotateRefreshToken(ctx, store.RotateInput{
		TokenHash: old, NewTokenHash: successor, TTL: 720 * time.Hour, Now: now,
	}); err != nil {
		t.Fatal(err)
	}

	var expires time.Time
	if err := db.Pool.QueryRow(ctx,
		`SELECT expires_at FROM refresh_token WHERE token_hash = $1`, successor).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if want := now.Add(720 * time.Hour); expires.Sub(want).Abs() > time.Second {
		t.Errorf("successor expires at %v, want a fresh 720h from the refresh (%v)", expires, want)
	}
}

func TestRefreshUnknownAndExpiredTokensAreInvalid(t *testing.T) {
	db, ctx := testDB(t)
	account := newAccount(t, db)

	if res := rotate(t, db, tokenHash(t), tokenHash(t)); res.Outcome != store.RotateInvalid {
		t.Errorf("unknown token = %+v, want invalid", res)
	}

	expired := tokenHash(t)
	family, err := db.InsertRefreshToken(ctx, account, expired, time.Now().Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if res := rotate(t, db, expired, tokenHash(t)); res.Outcome != store.RotateInvalid {
		t.Errorf("expired token = %+v, want invalid", res)
	}
	// Expiry is not reuse: nothing was written and no successor exists.
	var rows int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM refresh_token WHERE family_id = $1`, family).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("an expired refresh wrote %d rows in its family, want 1", rows)
	}
}

func TestRefreshConcurrentUseOfOneTokenRotatesOnceAndRevokesTheFamily(t *testing.T) {
	db, ctx := testDB(t)
	account := newAccount(t, db)
	token := tokenHash(t)
	family, err := db.InsertRefreshToken(ctx, account, token, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	const n = 8
	results := make([]store.RotateResult, n)
	errs := make([]error, n)
	successors := make([][]byte, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		successors[i] = tokenHash(t)
		wg.Go(func() {
			<-start
			results[i], errs[i] = db.RotateRefreshToken(ctx, store.RotateInput{
				TokenHash: token, NewTokenHash: successors[i], TTL: time.Hour, Now: time.Now(),
			})
		})
	}
	close(start)
	wg.Wait()

	rotated, reused := -1, 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("refresh %d: %v", i, errs[i])
		}
		switch results[i].Outcome {
		case store.RotateRotated:
			if rotated >= 0 {
				t.Fatalf("refreshes %d and %d both rotated the same token", rotated, i)
			}
			rotated = i
		case store.RotateReused:
			reused++
		default:
			t.Errorf("refresh %d = %+v, want rotated or reused", i, results[i])
		}
	}
	if rotated < 0 || reused != n-1 {
		t.Fatalf("rotated=%d reused=%d, want exactly one rotation and %d reuses", rotated, reused, n-1)
	}

	// The winner's successor was revoked by the losers' reuse detection.
	if res := rotate(t, db, successors[rotated], tokenHash(t)); res.Outcome != store.RotateRevoked {
		t.Errorf("winner's successor = %+v, want revoked", res)
	}
	var live int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM refresh_token WHERE family_id = $1 AND NOT revoked`, family).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Errorf("%d tokens still live after a concurrent reuse", live)
	}
}

func TestRevokeRefreshFamilyEndsOnlyThatFamily(t *testing.T) {
	db, ctx := testDB(t)
	account := newAccount(t, db)
	first, second, other := tokenHash(t), tokenHash(t), tokenHash(t)

	if _, err := db.InsertRefreshToken(ctx, account, first, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertRefreshToken(ctx, account, other, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	rotate(t, db, first, second)

	// Logging out with the spent first token still ends the family's live head.
	if err := db.RevokeRefreshFamily(ctx, first); err != nil {
		t.Fatalf("RevokeRefreshFamily: %v", err)
	}
	if res := rotate(t, db, second, tokenHash(t)); res.Outcome != store.RotateRevoked {
		t.Errorf("token after logout = %+v, want revoked", res)
	}

	// The same account's other session is untouched.
	if res := rotate(t, db, other, tokenHash(t)); res.Outcome != store.RotateRotated {
		t.Errorf("other family after logout = %+v, want rotated", res)
	}

	if err := db.RevokeRefreshFamily(ctx, tokenHash(t)); err != nil {
		t.Errorf("logout with an unknown token = %v, want nil", err)
	}
}

func TestRefreshTokenHashIsUnique(t *testing.T) {
	db, ctx := testDB(t)
	account := newAccount(t, db)
	h := tokenHash(t)
	if _, err := db.InsertRefreshToken(ctx, account, h, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertRefreshToken(ctx, account, h, time.Now().Add(time.Hour)); err == nil {
		t.Error("a duplicate token_hash was accepted")
	}
}

// A refresh racing another exchange of the same token, forced deterministically: a
// second transaction locks the row and spends it while RotateRefreshToken is in
// flight. When that transaction commits, the in-flight refresh must see a spent token
// and report reuse — never a second rotation.
func TestRefreshRacingAnExchangeReportsReuse(t *testing.T) {
	db, ctx := testDB(t)
	account := newAccount(t, db)
	token := tokenHash(t)
	family, err := db.InsertRefreshToken(ctx, account, token, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`SELECT 1 FROM refresh_token WHERE token_hash = $1 FOR UPDATE`, token); err != nil {
		t.Fatalf("lock row: %v", err)
	}

	type result struct {
		res store.RotateResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := db.RotateRefreshToken(ctx, store.RotateInput{
			TokenHash: token, NewTokenHash: tokenHash(t), TTL: time.Hour, Now: time.Now(),
		})
		done <- result{res, err}
	}()

	select {
	case r := <-done:
		t.Fatalf("RotateRefreshToken returned %+v while the row was locked", r)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := tx.Exec(ctx,
		`UPDATE refresh_token SET revoked = true, rotated_at = now() WHERE token_hash = $1`, token); err != nil {
		t.Fatalf("spend in the racing transaction: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("RotateRefreshToken: %v", r.err)
		}
		if r.res.Outcome != store.RotateReused {
			t.Errorf("refresh racing an exchange = %+v, want reused", r.res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RotateRefreshToken never returned")
	}

	var rows int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM refresh_token WHERE family_id = $1`, family).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("the losing refresh inserted a successor: %d rows in the family, want 1", rows)
	}
}
