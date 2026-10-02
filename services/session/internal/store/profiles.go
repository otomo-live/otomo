// Profiles: a player's public identity (SE-2; design/04-session-minimal.md
// SES-B1 and SES-B2).
//
// A profile is created once, on the first POST /me/init, with a provisional name and a
// random discriminator. Names are unique on (lower(display_name), discriminator), so both
// creating a profile and renaming one can collide with another player. Each collision is
// a unique violation that is retried with a new discriminator, a bounded number of times.
package store

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"uuid"
)

// discriminatorAttempts bounds the retries on a name collision. A name held by thousands
// of players could in theory keep colliding; after this many tries the caller is told the
// name is unavailable rather than looping.
const discriminatorAttempts = 10

// The largest discriminator. The column's CHECK is 1 to 9999, shown as four digits.
const maxDiscriminator = 9999

var (
	// ErrProfileNotFound: the player has no profile yet. They must call POST /me/init.
	ErrProfileNotFound = errors.New("store: profile not found")

	// ErrNameUnavailable: every discriminator tried for the name was taken.
	ErrNameUnavailable = errors.New("store: no free discriminator for that name")
)

// RenameTooSoonError is a rename inside the cooldown. RetryAt is when the next rename is
// allowed, taken from the database clock so every Session instance agrees on it.
type RenameTooSoonError struct {
	RetryAt time.Time
}

func (e *RenameTooSoonError) Error() string {
	return fmt.Sprintf("store: rename not allowed until %s", e.RetryAt.Format(time.RFC3339))
}

// Profile is one player_profile row.
type Profile struct {
	PlayerID      uuid.UUID
	DisplayName   string
	Discriminator int
	// NameChangedAt is the last rename, or nil when the player still has the name
	// POST /me/init gave them.
	NameChangedAt *time.Time
}

// randomDiscriminator picks a discriminator uniformly from the allowed range. It is not
// secret, so math/rand is enough. It is a variable so a test can force a collision.
var randomDiscriminator = func() int {
	return 1 + rand.IntN(maxDiscriminator)
}

// InitProfile returns the player's profile, creating it first if they have none. It is
// safe to call any number of times, from any number of requests at once.
//
// The insert is ON CONFLICT (player_id) DO NOTHING, so of several concurrent first calls
// exactly one inserts. The others wait for that insert to commit, insert nothing, and
// read the row it wrote. A unique violation can then only be the name index: another
// player already holds that name with that discriminator, and the insert is retried with
// a fresh provisional name and discriminator.
//
// created reports whether this call made the row.
func (d *DB) InitProfile(ctx context.Context, playerID uuid.UUID, provisional func() string) (p Profile, created bool, err error) {
	for range discriminatorAttempts {
		name, disc := provisional(), randomDiscriminator()

		var inserted bool
		err := d.Pool.QueryRow(ctx,
			`INSERT INTO player_profile (player_id, display_name, discriminator)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (player_id) DO NOTHING
			 RETURNING true`,
			playerID.String(), name, disc).Scan(&inserted)
		switch {
		case err == nil:
			return Profile{PlayerID: playerID, DisplayName: name, Discriminator: disc}, true, nil
		case errors.Is(err, pgx.ErrNoRows):
			// The player already had a profile, or a concurrent call just made it.
			p, err := d.GetProfile(ctx, playerID)
			return p, false, err
		case isUniqueViolation(err):
			continue
		default:
			return Profile{}, false, fmt.Errorf("store: insert profile: %w", err)
		}
	}
	return Profile{}, false, ErrNameUnavailable
}

// GetProfile reads the player's profile, or returns ErrProfileNotFound.
func (d *DB) GetProfile(ctx context.Context, playerID uuid.UUID) (Profile, error) {
	p := Profile{PlayerID: playerID}
	var disc int16
	err := d.Pool.QueryRow(ctx,
		`SELECT display_name, discriminator, name_changed_at
		   FROM player_profile WHERE player_id = $1`,
		playerID.String()).Scan(&p.DisplayName, &disc, &p.NameChangedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, ErrProfileNotFound
	}
	if err != nil {
		return Profile{}, fmt.Errorf("store: read profile: %w", err)
	}
	p.Discriminator = int(disc)
	return p, nil
}

// RenameProfile gives the player a new display name, which the caller has already
// checked against the name rules.
//
// The row is locked first, so two renames by one player cannot both pass the cooldown
// check. The player keeps their discriminator when the new name is free with it, and gets
// a random one otherwise. Each attempt runs in a savepoint, because a unique violation
// aborts the transaction it happens in.
//
// A name identical to the current one changes nothing and does not start the cooldown.
// A change of case only ("tanuki" to "Tanuki") is a real rename.
func (d *DB) RenameProfile(ctx context.Context, playerID uuid.UUID, name string, cooldown time.Duration) (Profile, error) {
	var out Profile
	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		cur := Profile{PlayerID: playerID}
		var disc int16
		var retryAt *time.Time
		err := tx.QueryRow(ctx,
			`SELECT display_name, discriminator, name_changed_at,
			        CASE WHEN name_changed_at + make_interval(secs => $2) > now()
			             THEN name_changed_at + make_interval(secs => $2) END
			   FROM player_profile WHERE player_id = $1
			    FOR UPDATE`,
			playerID.String(), cooldown.Seconds()).Scan(&cur.DisplayName, &disc, &cur.NameChangedAt, &retryAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrProfileNotFound
		}
		if err != nil {
			return fmt.Errorf("store: lock profile: %w", err)
		}
		cur.Discriminator = int(disc)

		if name == cur.DisplayName {
			out = cur
			return nil
		}
		if retryAt != nil {
			return &RenameTooSoonError{RetryAt: *retryAt}
		}

		next := cur.Discriminator
		for range discriminatorAttempts {
			var changedAt time.Time
			err := savepoint(ctx, tx, func(sp pgx.Tx) error {
				return sp.QueryRow(ctx,
					`UPDATE player_profile
					    SET display_name = $2, discriminator = $3,
					        name_changed_at = now(), updated_at = now()
					  WHERE player_id = $1
					RETURNING name_changed_at`,
					playerID.String(), name, next).Scan(&changedAt)
			})
			if isUniqueViolation(err) {
				next = randomDiscriminator()
				continue
			}
			if err != nil {
				return fmt.Errorf("store: rename profile: %w", err)
			}
			out = Profile{PlayerID: playerID, DisplayName: name, Discriminator: next, NameChangedAt: &changedAt}
			return nil
		}
		return ErrNameUnavailable
	})
	if err != nil {
		return Profile{}, err
	}
	return out, nil
}

// savepoint runs fn in a nested transaction (a SAVEPOINT), so a statement that fails
// inside it can be rolled back without aborting tx.
func savepoint(ctx context.Context, tx pgx.Tx, fn func(pgx.Tx) error) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(sp); err != nil {
		_ = sp.Rollback(ctx)
		return err
	}
	return sp.Commit(ctx)
}

// isUniqueViolation reports whether err is Postgres' unique_violation (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
