package store

import (
	"errors"
	"sync"
	"testing"
	"time"

	"uuid"
)

// freshPlayer returns a player id with no profile, and removes whatever the test creates
// for it, so a reused test database never carries rows from an earlier run into this one.
func freshPlayer(t *testing.T, db *DB) uuid.UUID {
	t.Helper()
	id := uuid.NewV7()
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(t.Context(), "delete from player_profile where player_id = $1", id.String())
	})
	return id
}

func fixedName(name string) func() string { return func() string { return name } }

// TestInitConcurrentCallsMakeOneProfile is SE-2's first acceptance criterion: ten
// concurrent POST /me/init calls for one player give exactly one profile, and every
// caller sees that same profile.
func TestInitConcurrentCallsMakeOneProfile(t *testing.T) {
	db := testDB(t)
	id := freshPlayer(t, db)

	const callers = 10
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		results = make([]Profile, callers)
		created = make([]bool, callers)
		errs    = make([]error, callers)
	)
	for i := range callers {
		wg.Go(func() {
			<-start
			results[i], created[i], errs[i] = db.InitProfile(t.Context(), id, fixedName("Player1234"))
		})
	}
	close(start)
	wg.Wait()

	madeIt := 0
	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if created[i] {
			madeIt++
		}
		if results[i].DisplayName != results[0].DisplayName || results[i].Discriminator != results[0].Discriminator {
			t.Errorf("caller %d saw %+v, caller 0 saw %+v", i, results[i], results[0])
		}
	}
	if madeIt != 1 {
		t.Errorf("%d callers reported creating the profile, want exactly 1", madeIt)
	}

	var rows int
	if err := db.Pool.QueryRow(t.Context(),
		"select count(*) from player_profile where player_id = $1", id.String()).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("%d profile rows, want 1", rows)
	}
}

// TestInitRetriesANameCollision: a provisional name and discriminator another player
// already holds is a unique violation, retried with a fresh pair.
func TestInitRetriesANameCollision(t *testing.T) {
	db := testDB(t)
	a, b := freshPlayer(t, db), freshPlayer(t, db)

	pa, _, err := db.InitProfile(t.Context(), a, fixedName("Collide"))
	if err != nil {
		t.Fatal(err)
	}

	// b asks for the same name, and its first discriminator is a's, so the first insert
	// is a unique violation on player_name_uq. The second is free.
	free := pa.Discriminator%maxDiscriminator + 1
	sequence := []int{pa.Discriminator, free}
	restore := randomDiscriminator
	t.Cleanup(func() { randomDiscriminator = restore })
	calls := 0
	randomDiscriminator = func() int {
		d := sequence[min(calls, len(sequence)-1)]
		calls++
		return d
	}

	pb, created, err := db.InitProfile(t.Context(), b, fixedName("Collide"))
	if err != nil || !created {
		t.Fatalf("InitProfile(b) = %+v, %v, %v", pb, created, err)
	}
	if calls != 2 || pb.Discriminator != free {
		t.Errorf("InitProfile(b) used %d discriminators and got #%d, want a retry to #%d", calls, pb.Discriminator, free)
	}
}

// TestInitGivesUpAfterBoundedCollisions: a name that collides on every attempt ends in
// ErrNameUnavailable rather than a loop.
func TestInitGivesUpAfterBoundedCollisions(t *testing.T) {
	db := testDB(t)
	a, b := freshPlayer(t, db), freshPlayer(t, db)

	pa, _, err := db.InitProfile(t.Context(), a, fixedName("Crowded"))
	if err != nil {
		t.Fatal(err)
	}
	restore := randomDiscriminator
	t.Cleanup(func() { randomDiscriminator = restore })
	randomDiscriminator = func() int { return pa.Discriminator }

	if _, _, err := db.InitProfile(t.Context(), b, fixedName("Crowded")); !errors.Is(err, ErrNameUnavailable) {
		t.Errorf("InitProfile with every attempt colliding = %v, want ErrNameUnavailable", err)
	}
}

func TestGetProfileNotFound(t *testing.T) {
	db := testDB(t)
	if _, err := db.GetProfile(t.Context(), freshPlayer(t, db)); !errors.Is(err, ErrProfileNotFound) {
		t.Errorf("GetProfile of a new player = %v, want ErrProfileNotFound", err)
	}
	if _, err := db.RenameProfile(t.Context(), freshPlayer(t, db), "Tanuki", time.Hour); !errors.Is(err, ErrProfileNotFound) {
		t.Errorf("RenameProfile of a new player = %v, want ErrProfileNotFound", err)
	}
}

// TestRenameTwiceInADayIsRefused is SE-2's second acceptance criterion. The first rename
// after init is free, because the provisional name does not start the cooldown.
func TestRenameTwiceInADayIsRefused(t *testing.T) {
	db := testDB(t)
	id := freshPlayer(t, db)
	if _, _, err := db.InitProfile(t.Context(), id, fixedName("Player0001")); err != nil {
		t.Fatal(err)
	}

	first, err := db.RenameProfile(t.Context(), id, "Tanuki", 24*time.Hour)
	if err != nil {
		t.Fatalf("first rename: %v", err)
	}
	if first.DisplayName != "Tanuki" || first.NameChangedAt == nil {
		t.Errorf("first rename = %+v", first)
	}

	_, err = db.RenameProfile(t.Context(), id, "Kitsune", 24*time.Hour)
	var tooSoon *RenameTooSoonError
	if !errors.As(err, &tooSoon) {
		t.Fatalf("second rename = %v, want RenameTooSoonError", err)
	}
	if d := time.Until(tooSoon.RetryAt); d < 23*time.Hour || d > 25*time.Hour {
		t.Errorf("RetryAt is %v away, want about 24h", d)
	}

	got, err := db.GetProfile(t.Context(), id)
	if err != nil || got.DisplayName != "Tanuki" {
		t.Errorf("after the refused rename the profile is %+v, %v; want Tanuki", got, err)
	}
}

// TestRenameToTheSameNameIsFree: sending the current name again changes nothing and does
// not use up the rename.
func TestRenameToTheSameNameIsFree(t *testing.T) {
	db := testDB(t)
	id := freshPlayer(t, db)
	p, _, err := db.InitProfile(t.Context(), id, fixedName("Player0002"))
	if err != nil {
		t.Fatal(err)
	}

	same, err := db.RenameProfile(t.Context(), id, p.DisplayName, 24*time.Hour)
	if err != nil || same.NameChangedAt != nil {
		t.Fatalf("rename to the same name = %+v, %v; want no change", same, err)
	}
	if _, err := db.RenameProfile(t.Context(), id, "Tanuki", 24*time.Hour); err != nil {
		t.Errorf("a real rename after the no-op was refused: %v", err)
	}
}

func TestRenameWithNoCooldown(t *testing.T) {
	db := testDB(t)
	id := freshPlayer(t, db)
	if _, _, err := db.InitProfile(t.Context(), id, fixedName("Player0003")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"One", "Two", "Three"} {
		if _, err := db.RenameProfile(t.Context(), id, name, 0); err != nil {
			t.Fatalf("rename to %s with no cooldown: %v", name, err)
		}
	}
}

// TestRenameKeepsOrReplacesTheDiscriminator: a player keeps their discriminator when the
// new name is free with it, and gets another when someone holds that pair, compared
// without case.
func TestRenameKeepsOrReplacesTheDiscriminator(t *testing.T) {
	db := testDB(t)
	a, b := freshPlayer(t, db), freshPlayer(t, db)

	pa, _, err := db.InitProfile(t.Context(), a, fixedName("Player0004"))
	if err != nil {
		t.Fatal(err)
	}
	pa, err = db.RenameProfile(t.Context(), a, "Tanuki", 0)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := db.InitProfile(t.Context(), b, fixedName("Player0005")); err != nil {
		t.Fatal(err)
	}
	// Give b the discriminator a holds, so renaming b to "tanuki" collides.
	if _, err := db.Pool.Exec(t.Context(),
		"update player_profile set discriminator = $2 where player_id = $1", b.String(), pa.Discriminator); err != nil {
		t.Fatal(err)
	}

	pb, err := db.RenameProfile(t.Context(), b, "tanuki", 0)
	if err != nil {
		t.Fatalf("colliding rename: %v", err)
	}
	if pb.Discriminator == pa.Discriminator {
		t.Errorf("b took %s#%d while a holds Tanuki#%d", pb.DisplayName, pb.Discriminator, pa.Discriminator)
	}

	// A rename with no collision keeps the discriminator.
	kept, err := db.RenameProfile(t.Context(), a, "Kitsune", 0)
	if err != nil || kept.Discriminator != pa.Discriminator {
		t.Errorf("free rename = %+v, %v; want discriminator %d kept", kept, err, pa.Discriminator)
	}
}
