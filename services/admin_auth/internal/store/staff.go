// Queries for staff accounts and the login path's writes: the failure counter and the
// success transaction that opens a refresh session and records the audit trail.
//
// Every multi-row write here happens in one transaction. The audit row is part of the
// change it describes, so a crash cannot leave a login unrecorded, and the lockout
// update is one statement, so two concurrent wrong passwords cannot both read the
// same failed_logins value and each write failed_logins+1.

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"uuid"

	"github.com/jackc/pgx/v5"
)

// User is one staff_user row as the login path needs it. LockedUntil and
// TOTPConfirmedAt are pointers because the columns are nullable; nil means "never
// locked" and "no confirmed TOTP" respectively.
type User struct {
	ID              string
	Name            string
	Email           string
	PasswordHash    string
	Roles           []string
	IsRoot          bool
	Status          string
	LockedUntil     *time.Time
	TOTPConfirmedAt *time.Time
}

// Entry is the acting identity written into audit_log. The action and target belong to
// the operation, not to the actor, so they are arguments where the audit row is made.
type Entry struct {
	ID   string
	Name string
}

// UserByEmail returns the account whose email matches, case-insensitively, or an error
// wrapping ErrNotFound. The case-insensitive form is the one the unique index is built
// on, so this query uses it too and a login never depends on the stored casing.
func (d *DB) UserByEmail(ctx context.Context, email string) (User, error) {
	var (
		u             User
		lockedUntil   *time.Time
		totpConfirmed *time.Time
	)
	err := d.Pool.QueryRow(ctx,
		`SELECT id::text, name, email, password_hash, roles, is_root, status, locked_until, totp_confirmed_at
		 FROM staff_user
		 WHERE lower(email) = lower($1)`,
		email,
	).Scan(&u.ID, &u.Name, &u.Email, &u.PasswordHash, &u.Roles, &u.IsRoot, &u.Status, &lockedUntil, &totpConfirmed)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, fmt.Errorf("staff user by email: %w", ErrNotFound)
	}
	if err != nil {
		return User{}, fmt.Errorf("staff user by email: %w", err)
	}
	u.LockedUntil = lockedUntil
	u.TOTPConfirmedAt = totpConfirmed
	return u, nil
}

// RecordLoginFailure increments the failure counter in one atomic UPDATE and reports
// whether this call reached maxFailures and locked the account. When it locks, it
// resets failed_logins to 0 and sets locked_until to now+lockFor; a locked account
// therefore starts a fresh count when the lock expires. The lock event's audit row is
// written in the same transaction.
func (d *DB) RecordLoginFailure(ctx context.Context, id string, maxFailures int, lockFor time.Duration) (bool, error) {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("record login failure for %s: begin: %w", id, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		name   string
		locked bool
	)
	err = tx.QueryRow(ctx,
		`UPDATE staff_user
		 SET failed_logins = CASE WHEN failed_logins + 1 >= $2 THEN 0 ELSE failed_logins + 1 END,
		     locked_until  = CASE WHEN failed_logins + 1 >= $2 THEN now() + make_interval(secs => $3) ELSE locked_until END,
		     updated_at    = now()
		 WHERE id = $1::uuid
		 RETURNING name, (failed_logins = 0 AND locked_until > now())`,
		id, maxFailures, lockFor.Seconds(),
	).Scan(&name, &locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("record login failure for %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return false, fmt.Errorf("record login failure for %s: %w", id, err)
	}

	if locked {
		if err := insertAudit(ctx, tx, Entry{ID: id, Name: name}, "login.locked", id); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("record login failure for %s: commit: %w", id, err)
	}
	return locked, nil
}

// RecordLoginSuccess clears the failure counter and lock, stamps last_login_at, opens
// one refresh_session row and writes the login.success audit row — all in one
// transaction, so no partial success can survive.
//
// refreshHash is the sha256 of the refresh token, never the token itself: a database
// reader must not be able to replay a session. familyID names the rotation chain the
// session starts. details is written into the login.success audit row: the password
// path passes nil (the row keeps its '{}' default) and the MFA paths record which
// factor was used.
func (d *DB) RecordLoginSuccess(ctx context.Context, id, ip, ua string, refreshHash []byte, familyID uuid.UUID, refreshTTL time.Duration, actor Entry, details map[string]any) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("record login success for %s: begin: %w", id, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		`UPDATE staff_user
		 SET failed_logins = 0, locked_until = NULL, last_login_at = now(), updated_at = now()
		 WHERE id = $1::uuid`,
		id)
	if err != nil {
		return fmt.Errorf("record login success for %s: update: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("record login success for %s: %w", id, ErrNotFound)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO refresh_session (family_id, user_id, token_hash, expires_at, ip, user_agent)
		 VALUES ($1::uuid, $2::uuid, $3, now() + make_interval(secs => $4), $5, $6)`,
		familyID.String(), id, refreshHash, refreshTTL.Seconds(), ip, ua); err != nil {
		return fmt.Errorf("record login success for %s: insert refresh session: %w", id, err)
	}

	if details == nil {
		details = map[string]any{}
	}
	if err := insertAuditDetails(ctx, tx, actor, "login.success", id, details); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("record login success for %s: commit: %w", id, err)
	}
	return nil
}

// insertAudit appends one audit_log row inside the caller's transaction. details keeps
// its '{}' default: nothing on the login path has structured detail worth recording
// yet, and the action plus target already say what happened to whom.
func insertAudit(ctx context.Context, tx pgx.Tx, actor Entry, action, target string) error {
	if _, err := tx.Exec(ctx,
		`INSERT INTO audit_log (actor_id, actor_name, action, target) VALUES ($1, $2, $3, $4)`,
		actor.ID, actor.Name, action, target); err != nil {
		return fmt.Errorf("write audit %s: %w", action, err)
	}
	return nil
}
