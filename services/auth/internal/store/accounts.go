// Queries for account and identity_binding: the find-or-create behind every login.

package store

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/jackc/pgx/v5"
)

// MethodDevice is the identity_binding.method for anonymous device logins. Its
// external_id is the hex SHA-256 of the client's device_id, never the device_id itself:
// the device_id is the player's only credential, so a copy of this table must not be
// enough to log in as anyone.
const MethodDevice = "device"

// FindOrCreateAccount returns the account bound to (method, externalID), creating the
// account and its binding when there is none. The returned ID is the player's `sub`,
// and it is the same on every call for the same binding — Session keys every row it
// owns on it, so a second account for one device would orphan that player's data.
//
// Two first logins for one binding can race. Both miss the lookup and both insert an
// account, but only one binding insert can win the primary key: the loser's ON CONFLICT
// DO NOTHING waits for the winner to commit and then affects no row. The loser rolls
// back, which discards its account row too, and reads the winner's binding instead.
// Either way exactly one account exists and every caller gets its ID. created is true
// only for the caller whose insert won (AU-6's auth_logins_total{new_account}).
func (d *DB) FindOrCreateAccount(ctx context.Context, method, externalID string) (id string, created bool, err error) {
	if id, err := d.findAccount(ctx, method, externalID); err == nil {
		return id, false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return "", false, err
	}

	newID, err := d.createAccount(ctx, method, externalID)
	if err != nil {
		return "", false, err
	}
	if newID != "" {
		return newID, true, nil
	}

	// Lost the race: the winner has committed by now, so its binding is visible.
	id, err = d.findAccount(ctx, method, externalID)
	return id, false, err
}

// findAccount returns the account bound to (method, externalID), or an error wrapping
// ErrNotFound.
func (d *DB) findAccount(ctx context.Context, method, externalID string) (string, error) {
	var id string
	err := d.Pool.QueryRow(ctx,
		`SELECT account_id::text FROM identity_binding WHERE method = $1 AND external_id = $2`,
		method, externalID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("identity binding %s: %w", method, ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("find account by %s binding: %w", method, err)
	}
	return id, nil
}

// createAccount inserts a new account and its binding in one transaction and returns
// the new account ID, or "" with a nil error when a concurrent login created the
// binding first — in which case nothing was written.
func (d *DB) createAccount(ctx context.Context, method, externalID string) (string, error) {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("create account: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	id := uuid.New().String()
	if _, err := tx.Exec(ctx, `INSERT INTO account (account_id) VALUES ($1)`, id); err != nil {
		return "", fmt.Errorf("create account: insert account: %w", err)
	}
	tag, err := tx.Exec(ctx,
		`INSERT INTO identity_binding (method, external_id, account_id) VALUES ($1, $2, $3)
		 ON CONFLICT (method, external_id) DO NOTHING`,
		method, externalID, id)
	if err != nil {
		return "", fmt.Errorf("create account: insert %s binding: %w", method, err)
	}
	if tag.RowsAffected() == 0 {
		return "", nil // the deferred rollback discards the account row
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("create account: commit: %w", err)
	}
	return id, nil
}
