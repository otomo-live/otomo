// Queries for the single break-glass root account: the one staff_user row with
// is_root = true. It is password-only and its only job is creating personal admin
// accounts, so this file has exactly the three operations bootstrap-root needs —
// read the row, insert it, rotate its password — each writing its audit row in the
// same transaction as the change.
//
// The database enforces at most one root through staff_user_single_root_idx, a
// unique index over a constant expression restricted to root rows. CreateRoot turns
// that index's 23505 into ErrRootExists so a second concurrent bootstrap can report
// "already exists" instead of racing a check-then-insert.

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrRootExists reports that a root row already exists. It is the single-root unique
// index's 23505, not the case-insensitive email index's, so callers can distinguish a
// concurrent bootstrap from an address that belongs to a personal account.
var ErrRootExists = errors.New("root account already exists")

// RootUser is the root row as bootstrap-root needs it.
type RootUser struct {
	ID           string
	Email        string
	Name         string
	PasswordHash string
}

// Root returns the single root account, or an error wrapping ErrNotFound when none
// exists. This is the pre-flight read; the insert itself still relies on the unique
// index, because this lookup and the insert are not one atomic step.
func (d *DB) Root(ctx context.Context) (RootUser, error) {
	var u RootUser
	err := d.Pool.QueryRow(ctx,
		`SELECT id::text, email, name, password_hash FROM staff_user WHERE is_root`,
	).Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return RootUser{}, fmt.Errorf("root account: %w", ErrNotFound)
	}
	if err != nil {
		return RootUser{}, fmt.Errorf("root account: %w", err)
	}
	return u, nil
}

// CreateRoot inserts the root row and its root.bootstrap audit row in one
// transaction, returning the new id. The email is lowercased before it is stored so
// the stored form matches the case-insensitive login lookup. created_by is left NULL,
// which the schema reserves for exactly this row.
//
// Any 23505 is reported as ErrRootExists when a root row is present afterwards: the
// insert may have lost the single-root race, or a concurrent run with the same address
// may have won the email race. When no root exists the 23505 is a genuine address
// collision with a personal account and is returned as-is.
func (d *DB) CreateRoot(ctx context.Context, email, name, passwordHash string) (string, error) {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("create root: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id string
	err = tx.QueryRow(ctx,
		`INSERT INTO staff_user (email, name, password_hash, roles, is_root)
		 VALUES ($1, $2, $3, ARRAY['admin'], true)
		 RETURNING id::text`,
		strings.ToLower(email), name, passwordHash,
	).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// The insert lost a race. Roll the aborted transaction back before
			// consulting the pool, then report ErrRootExists only when a root row
			// really is present: a 23505 against the email index alone (a personal
			// account already owns the address) is a genuine collision, not a
			// concurrent bootstrap.
			_ = tx.Rollback(ctx)
			var roots int
			if qErr := d.Pool.QueryRow(ctx, `SELECT count(*) FROM staff_user WHERE is_root`).Scan(&roots); qErr == nil && roots > 0 {
				return "", ErrRootExists
			}
		}
		return "", fmt.Errorf("create root: insert: %w", err)
	}

	if err := insertAudit(ctx, tx, Entry{ID: "bootstrap", Name: "bootstrap"}, "root.bootstrap", id); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("create root: commit: %w", err)
	}
	return id, nil
}

// RotateRoot replaces the root password and revokes every live refresh session it
// owns in one transaction, returning how many sessions were revoked. failed_logins
// and locked_until are reset as part of the same statement, so a rotation also clears
// a lockout: this is the break-glass path and it must leave the account usable.
func (d *DB) RotateRoot(ctx context.Context, id, passwordHash string) (int64, error) {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("rotate root: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		`UPDATE staff_user
		 SET password_hash = $2, password_changed_at = now(),
		     failed_logins = 0, locked_until = NULL, updated_at = now()
		 WHERE id = $1::uuid`,
		id, passwordHash)
	if err != nil {
		return 0, fmt.Errorf("rotate root: update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return 0, fmt.Errorf("rotate root: %w", ErrNotFound)
	}

	revoked, err := tx.Exec(ctx,
		`UPDATE refresh_session SET revoked_at = $2 WHERE user_id = $1::uuid AND revoked_at IS NULL`,
		id, time.Now())
	if err != nil {
		return 0, fmt.Errorf("rotate root: revoke sessions: %w", err)
	}

	if err := insertAudit(ctx, tx, Entry{ID: "bootstrap", Name: "bootstrap"}, "root.rotate", id); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("rotate root: commit: %w", err)
	}
	return revoked.RowsAffected(), nil
}
