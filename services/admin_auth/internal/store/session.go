// Queries for the refresh-session lifecycle: the rotation performed by
// POST /admin-auth/refresh, the family revocation behind logout, reuse detection and
// a disabled account, and the single-row lookup /me uses after verifying a token.
//
// Every write happens in one transaction that includes the audit row describing it,
// so a crash cannot revoke a family without recording why. The refresh lookup takes
// the row FOR UPDATE, which is what serialises simultaneous refreshes of one cookie:
// the first rotates it, the rest read the rotated row and take the benign-race path
// instead of each minting a successor.

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// RefreshOutcome is what RefreshSession decided about one presented token.
type RefreshOutcome int

const (
	// RefreshNoSession: no row, or the row is expired/revoked. The caller answers
	// the same 401 as every other unusable cookie.
	RefreshNoSession RefreshOutcome = iota
	// RefreshReused: the token was exchanged longer ago than the grace window, so
	// this presentation is treated as theft; the whole family is revoked.
	RefreshReused
	// RefreshInactive: the session is fine but its user is disabled; the family is
	// revoked.
	RefreshInactive
	// RefreshRotated: the normal path. The old row was stamped rotated_at and a
	// successor was inserted, so the caller must send the new cookie.
	RefreshRotated
	// RefreshGrace: the token was rotated within the grace window, which is the
	// benign two-tab race. No row was written and the caller sends no cookie.
	RefreshGrace
)

// RefreshInput is everything one refresh needs to write. TokenHash and NewTokenHash
// are sha256 digests, never the raw cookie values.
type RefreshInput struct {
	TokenHash    []byte
	NewTokenHash []byte
	RefreshTTL   time.Duration
	ReuseGrace   time.Duration
	Now          time.Time
	IP           string
	UserAgent    string
}

// RefreshResult is the identity the handler needs to mint the next access token,
// re-read from staff_user at refresh time so a demotion takes effect immediately.
type RefreshResult struct {
	Outcome RefreshOutcome
	UserID  string
	Name    string
	Roles   []string
}

// RefreshSession looks up one refresh token FOR UPDATE and decides what happens to
// it, all in a single transaction.
//
// The order of the checks is part of the contract: a revoked or expired row is dead
// no matter what else is true; an already-rotated row is either the benign race or
// reuse depending only on the grace window; and only a live, unrotated row reaches
// the account-status check.
func (d *DB) RefreshSession(ctx context.Context, in RefreshInput) (RefreshResult, error) {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return RefreshResult{}, fmt.Errorf("refresh session: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		id, familyID, userID string
		name, status         string
		roles                []string
		rotatedAt            *time.Time
		revokedAt            *time.Time
		expiresAt            time.Time
	)
	err = tx.QueryRow(ctx,
		`SELECT rs.id::text, rs.family_id::text, rs.user_id::text,
		        rs.rotated_at, rs.revoked_at, rs.expires_at,
		        su.name, su.roles, su.status
		 FROM refresh_session rs
		 JOIN staff_user su ON su.id = rs.user_id
		 WHERE rs.token_hash = $1
		 FOR UPDATE OF rs`,
		in.TokenHash,
	).Scan(&id, &familyID, &userID, &rotatedAt, &revokedAt, &expiresAt, &name, &roles, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return RefreshResult{}, fmt.Errorf("refresh session: commit empty: %w", err)
		}
		return RefreshResult{Outcome: RefreshNoSession}, nil
	}
	if err != nil {
		return RefreshResult{}, fmt.Errorf("refresh session: lookup: %w", err)
	}

	result := RefreshResult{UserID: userID, Name: name, Roles: roles}

	if revokedAt != nil || !expiresAt.After(in.Now) {
		if err := tx.Commit(ctx); err != nil {
			return RefreshResult{}, fmt.Errorf("refresh session: commit dead: %w", err)
		}
		result.Outcome = RefreshNoSession
		return result, nil
	}

	if status != "active" {
		// A disabled account must not be able to keep a session alive by refreshing.
		// This runs before the grace branch below on purpose: a replay inside the
		// grace window would otherwise still mint an access token for an account
		// that was disabled a moment ago.
		if err := revokeFamily(ctx, tx, familyID); err != nil {
			return RefreshResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return RefreshResult{}, fmt.Errorf("refresh session: commit inactive: %w", err)
		}
		result.Outcome = RefreshInactive
		return result, nil
	}

	if rotatedAt != nil {
		if in.ReuseGrace > 0 && !in.Now.After(rotatedAt.Add(in.ReuseGrace)) {
			// A benign multi-tab race: both tabs' access tokens expired together
			// and both refreshed with the same cookie. The second request must
			// succeed but must not rotate again — the browser is already holding
			// the successor cookie. Without this window the second tab would look
			// like theft and log the admin out of every tab.
			if err := tx.Commit(ctx); err != nil {
				return RefreshResult{}, fmt.Errorf("refresh session: commit grace: %w", err)
			}
			result.Outcome = RefreshGrace
			return result, nil
		}

		// Reuse outside the grace window: someone replayed an exchanged token.
		// Revoke every live session in the family and record why, in this tx.
		if err := revokeFamily(ctx, tx, familyID); err != nil {
			return RefreshResult{}, err
		}
		if err := insertAuditDetails(ctx, tx, Entry{ID: userID, Name: name}, "session.reuse_detected", userID,
			map[string]string{"family_id": familyID}); err != nil {
			return RefreshResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return RefreshResult{}, fmt.Errorf("refresh session: commit reuse: %w", err)
		}
		result.Outcome = RefreshReused
		return result, nil
	}

	if _, err := tx.Exec(ctx,
		`UPDATE refresh_session SET rotated_at = $2 WHERE id = $1::uuid`,
		id, in.Now); err != nil {
		return RefreshResult{}, fmt.Errorf("refresh session: stamp rotated: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO refresh_session (family_id, user_id, token_hash, expires_at, ip, user_agent)
		 VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6)`,
		familyID, userID, in.NewTokenHash, in.Now.Add(in.RefreshTTL), in.IP, in.UserAgent); err != nil {
		return RefreshResult{}, fmt.Errorf("refresh session: insert successor: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return RefreshResult{}, fmt.Errorf("refresh session: commit rotate: %w", err)
	}
	result.Outcome = RefreshRotated
	return result, nil
}

// Logout revokes the whole family behind one refresh cookie and records the action.
// A cookie that matches no row is not an error: logout is idempotent, and the
// caller still answers 204.
func (d *DB) Logout(ctx context.Context, tokenHash []byte) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("logout: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		userID, name, familyID string
	)
	err = tx.QueryRow(ctx,
		`SELECT rs.user_id::text, su.name, rs.family_id::text
		 FROM refresh_session rs
		 JOIN staff_user su ON su.id = rs.user_id
		 WHERE rs.token_hash = $1
		 FOR UPDATE OF rs`,
		tokenHash,
	).Scan(&userID, &name, &familyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return tx.Commit(ctx)
	}
	if err != nil {
		return fmt.Errorf("logout: lookup: %w", err)
	}

	if err := revokeFamily(ctx, tx, familyID); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, Entry{ID: userID, Name: name}, "logout", userID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("logout: commit: %w", err)
	}
	return nil
}

// revokeFamily marks every live row in the family revoked. Rows already rotated or
// revoked are left as they are; only a session that could still be refreshed needs
// killing.
func revokeFamily(ctx context.Context, tx pgx.Tx, familyID string) error {
	if _, err := tx.Exec(ctx,
		`UPDATE refresh_session SET revoked_at = now() WHERE family_id = $1::uuid AND revoked_at IS NULL`,
		familyID); err != nil {
		return fmt.Errorf("revoke refresh family %s: %w", familyID, err)
	}
	return nil
}

// insertAuditDetails is insertAudit with a structured details object. The login path
// keeps its '{}' default; reuse detection needs the family it revoked so an operator
// can trace which chain was killed.
func insertAuditDetails(ctx context.Context, tx pgx.Tx, actor Entry, action, target string, details any) error {
	raw, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("encode audit %s details: %w", action, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO audit_log (actor_id, actor_name, action, target, details)
		 VALUES ($1, $2, $3, $4, $5::jsonb)`,
		actor.ID, actor.Name, action, target, raw); err != nil {
		return fmt.Errorf("write audit %s: %w", action, err)
	}
	return nil
}

// UserByID returns one staff account by primary key, or an error wrapping
// ErrNotFound. /me uses it after verifying a token so name and roles always come
// from the row a disabled account can be seen in, never from the token's claims.
func (d *DB) UserByID(ctx context.Context, id string) (User, error) {
	var (
		u             User
		lockedUntil   *time.Time
		totpConfirmed *time.Time
	)
	err := d.Pool.QueryRow(ctx,
		`SELECT id::text, name, email, password_hash, roles, is_root, status, locked_until, totp_confirmed_at
		 FROM staff_user
		 WHERE id = $1::uuid`,
		id,
	).Scan(&u.ID, &u.Name, &u.Email, &u.PasswordHash, &u.Roles, &u.IsRoot, &u.Status, &lockedUntil, &totpConfirmed)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, fmt.Errorf("staff user by id: %w", ErrNotFound)
	}
	if err != nil {
		return User{}, fmt.Errorf("staff user by id: %w", err)
	}
	u.LockedUntil = lockedUntil
	u.TOTPConfirmedAt = totpConfirmed
	return u, nil
}
