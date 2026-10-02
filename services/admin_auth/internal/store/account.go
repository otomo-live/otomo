// Queries behind the self-service account endpoints under /admin-auth/account.
//
// These are the only writes a signed-in staff member makes to their own row. Every
// one happens in a single transaction together with the audit row that describes it,
// and every one that can race a concurrent request takes the staff_user row FOR
// UPDATE first, so a second request sees the state the first left rather than the
// state it read.

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrMFARequired reports an attempt by an admin account to disable its second
// factor, which decision D5 forbids: admins must keep a confirmed factor.
var ErrMFARequired = errors.New("mfa is required for admins")

// AccountSession is one live refresh session as the account page lists it. It is the
// head of a rotation family: ID names the row the browser currently holds,
// CreatedAt is when the login started, and LastUsedAt is when the most recent
// rotation minted the head — the closest thing to a last-used time the schema has,
// because refresh_session has no dedicated column for one.
type AccountSession struct {
	ID         string
	CreatedAt  time.Time
	LastUsedAt time.Time
	IP         *string
	UserAgent  *string
	Current    bool
}

// RevokeSessionsResult is what a revoke-others call did, for the response body and
// the audit details.
type RevokeSessionsResult struct {
	// Revoked is how many live rows were marked revoked.
	Revoked int
	// CurrentKept is true when a supplied cookie named a live session of this user
	// and its family was left alone.
	CurrentKept bool
}

// ChangePassword replaces the account's password hash, revokes its other live
// refresh sessions and writes account.password_changed — all in one transaction.
//
// keepTokenHash, when non-nil, is the sha256 of the caller's refresh cookie. The
// family that token belongs to is left alive; every other live family and every
// unmatched row is revoked. A cookie that names no row of this user is ignored, so
// the change still revokes everything.
func (d *DB) ChangePassword(ctx context.Context, userID, newHash string, keepTokenHash []byte, actor Entry) (int, error) {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("change password: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		`UPDATE staff_user
		    SET password_hash = $2, password_changed_at = now(), updated_at = now()
		  WHERE id = $1::uuid`,
		userID, newHash)
	if err != nil {
		return 0, fmt.Errorf("change password: update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return 0, fmt.Errorf("change password: %w", ErrNotFound)
	}

	keepFamily := ""
	if len(keepTokenHash) > 0 {
		err := tx.QueryRow(ctx,
			`SELECT family_id::text FROM refresh_session WHERE token_hash = $1 AND user_id = $2::uuid`,
			keepTokenHash, userID).Scan(&keepFamily)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("change password: resolve current family: %w", err)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			keepFamily = ""
		}
	}

	var revoked int
	if keepFamily == "" {
		tag, err = tx.Exec(ctx,
			`UPDATE refresh_session SET revoked_at = now()
			  WHERE user_id = $1::uuid AND revoked_at IS NULL`,
			userID)
	} else {
		tag, err = tx.Exec(ctx,
			`UPDATE refresh_session SET revoked_at = now()
			  WHERE user_id = $1::uuid AND revoked_at IS NULL AND family_id <> $2::uuid`,
			userID, keepFamily)
	}
	if err != nil {
		return 0, fmt.Errorf("change password: revoke sessions: %w", err)
	}
	revoked = int(tag.RowsAffected())

	if err := insertAuditDetails(ctx, tx, actor, "account.password_changed", userID,
		map[string]any{"sessions_revoked": revoked}); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("change password: commit: %w", err)
	}
	return revoked, nil
}

// ListAccountSessions returns the caller's live sessions, newest last-used first.
// A session is the head of a rotation family — the row whose token the browser
// actually holds — because the rotated predecessors in the same family are not
// separately sign-out-able. currentTokenHash, when non-nil, marks the matching row.
func (d *DB) ListAccountSessions(ctx context.Context, userID string, currentTokenHash []byte) ([]AccountSession, error) {
	rows, err := d.Pool.Query(ctx,
		`SELECT rs.id::text,
		        COALESCE((SELECT min(f.created_at) FROM refresh_session f WHERE f.family_id = rs.family_id), rs.created_at),
		        rs.created_at,
		        rs.ip, rs.user_agent,
		        ($2::bytea IS NOT NULL AND rs.token_hash = $2)
		   FROM refresh_session rs
		  WHERE rs.user_id = $1::uuid
		    AND rs.rotated_at IS NULL
		    AND rs.revoked_at IS NULL
		    AND rs.expires_at > now()
		  ORDER BY rs.created_at DESC`,
		userID, currentTokenHash)
	if err != nil {
		return nil, fmt.Errorf("list account sessions: %w", err)
	}
	defer rows.Close()

	var sessions []AccountSession
	for rows.Next() {
		var s AccountSession
		if err := rows.Scan(&s.ID, &s.CreatedAt, &s.LastUsedAt, &s.IP, &s.UserAgent, &s.Current); err != nil {
			return nil, fmt.Errorf("list account sessions: scan: %w", err)
		}
		sessions = append(sessions, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list account sessions: %w", err)
	}
	return sessions, nil
}

// RevokeOtherSessions revokes every live refresh session of userID except the family
// named by currentTokenHash, and writes account.sessions_revoked. A nil or unknown
// token revokes everything and reports CurrentKept false.
func (d *DB) RevokeOtherSessions(ctx context.Context, userID string, currentTokenHash []byte, actor Entry) (RevokeSessionsResult, error) {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return RevokeSessionsResult{}, fmt.Errorf("revoke other sessions: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	keepFamily := ""
	if len(currentTokenHash) > 0 {
		err := tx.QueryRow(ctx,
			`SELECT family_id::text FROM refresh_session WHERE token_hash = $1 AND user_id = $2::uuid`,
			currentTokenHash, userID).Scan(&keepFamily)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return RevokeSessionsResult{}, fmt.Errorf("revoke other sessions: resolve current family: %w", err)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			keepFamily = ""
		}
	}

	var revoked int
	if keepFamily == "" {
		tag, err := tx.Exec(ctx,
			`UPDATE refresh_session SET revoked_at = now()
			  WHERE user_id = $1::uuid AND revoked_at IS NULL`,
			userID)
		if err != nil {
			return RevokeSessionsResult{}, fmt.Errorf("revoke other sessions: revoke all: %w", err)
		}
		revoked = int(tag.RowsAffected())
	} else {
		tag, err := tx.Exec(ctx,
			`UPDATE refresh_session SET revoked_at = now()
			  WHERE user_id = $1::uuid AND revoked_at IS NULL AND family_id <> $2::uuid`,
			userID, keepFamily)
		if err != nil {
			return RevokeSessionsResult{}, fmt.Errorf("revoke other sessions: revoke others: %w", err)
		}
		revoked = int(tag.RowsAffected())
	}
	result := RevokeSessionsResult{Revoked: revoked, CurrentKept: keepFamily != ""}

	if err := insertAuditDetails(ctx, tx, actor, "account.sessions_revoked", userID, map[string]any{
		"revoked":      result.Revoked,
		"current_kept": result.CurrentKept,
	}); err != nil {
		return RevokeSessionsResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RevokeSessionsResult{}, fmt.Errorf("revoke other sessions: commit: %w", err)
	}
	return result, nil
}

// ReplaceRecoveryCodes swaps every stored recovery code for hashes, advancing the
// TOTP replay guard to step in the same transaction and writing
// mfa.recovery_regenerated. An account without a confirmed factor reports
// ErrMFANotEnrolled; a step not newer than the stored one reports ErrTOTPReplay.
func (d *DB) ReplaceRecoveryCodes(ctx context.Context, userID string, step int64, hashes [][]byte, actor Entry) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("replace recovery codes: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var confirmedAt *time.Time
	err = tx.QueryRow(ctx,
		`SELECT totp_confirmed_at FROM staff_user WHERE id = $1::uuid FOR UPDATE`,
		userID).Scan(&confirmedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("replace recovery codes: %w", ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("replace recovery codes: lookup: %w", err)
	}
	if confirmedAt == nil {
		return ErrMFANotEnrolled
	}

	tag, err := tx.Exec(ctx,
		`UPDATE staff_user SET totp_last_step = $2, updated_at = now()
		  WHERE id = $1::uuid AND (totp_last_step IS NULL OR totp_last_step < $2)`,
		userID, step)
	if err != nil {
		return fmt.Errorf("replace recovery codes: advance step: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTOTPReplay
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM mfa_recovery_code WHERE user_id = $1::uuid`, userID); err != nil {
		return fmt.Errorf("replace recovery codes: delete: %w", err)
	}
	for _, hash := range hashes {
		if _, err := tx.Exec(ctx,
			`INSERT INTO mfa_recovery_code (user_id, code_hash) VALUES ($1::uuid, $2)`,
			userID, hash); err != nil {
			return fmt.Errorf("replace recovery codes: insert: %w", err)
		}
	}

	if err := insertAuditDetails(ctx, tx, actor, "mfa.recovery_regenerated", userID,
		map[string]any{"count": len(hashes)}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("replace recovery codes: commit: %w", err)
	}
	return nil
}

// DisableMFA clears a non-admin account's second factor, recovery codes, pending
// secret, replay step and open MFA tickets, and writes mfa.disabled, all in one
// transaction under the user row lock.
//
// Exactly one of step and recoveryHash identifies the proof the caller supplied:
// step is the TOTP step that matched and is subject to the replay guard, while
// recoveryHash is the sha256 of a normalized recovery code that must exist unused.
// The rule checks are repeated here — root protected, admin required to keep MFA,
// no factor to disable — so a caller cannot skip them by racing the handler's
// pre-check.
func (d *DB) DisableMFA(ctx context.Context, userID string, step *int64, recoveryHash []byte, actor Entry) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("disable mfa: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		roles       []string
		isRoot      bool
		confirmedAt *time.Time
		secret      []byte
	)
	err = tx.QueryRow(ctx,
		`SELECT roles, is_root, totp_confirmed_at, totp_secret_enc
		 FROM staff_user WHERE id = $1::uuid FOR UPDATE`,
		userID).Scan(&roles, &isRoot, &confirmedAt, &secret)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("disable mfa: %w", ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("disable mfa: lookup: %w", err)
	}
	if isRoot {
		return ErrRootProtected
	}
	if HasRole(roles, adminRole) {
		return ErrMFARequired
	}

	var recoveryCount int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM mfa_recovery_code WHERE user_id = $1::uuid`, userID).Scan(&recoveryCount); err != nil {
		return fmt.Errorf("disable mfa: count recovery codes: %w", err)
	}
	if confirmedAt == nil && secret == nil && recoveryCount == 0 {
		return ErrMFANotEnrolled
	}

	switch {
	case step != nil:
		tag, err := tx.Exec(ctx,
			`UPDATE staff_user SET totp_last_step = $2, updated_at = now()
			  WHERE id = $1::uuid AND totp_confirmed_at IS NOT NULL
			    AND (totp_last_step IS NULL OR totp_last_step < $2)`,
			userID, *step)
		if err != nil {
			return fmt.Errorf("disable mfa: advance step: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrTOTPReplay
		}
	case recoveryHash != nil:
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM mfa_recovery_code
			                 WHERE user_id = $1::uuid AND code_hash = $2 AND used_at IS NULL)`,
			userID, recoveryHash).Scan(&exists); err != nil {
			return fmt.Errorf("disable mfa: check recovery code: %w", err)
		}
		if !exists {
			return ErrRecoveryCodeInvalid
		}
	default:
		return ErrRecoveryCodeInvalid
	}

	if _, err := tx.Exec(ctx,
		`UPDATE staff_user
		    SET totp_secret_enc = NULL, totp_confirmed_at = NULL, totp_last_step = NULL, updated_at = now()
		  WHERE id = $1::uuid`,
		userID); err != nil {
		return fmt.Errorf("disable mfa: clear factor: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM mfa_recovery_code WHERE user_id = $1::uuid`, userID); err != nil {
		return fmt.Errorf("disable mfa: delete recovery codes: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE mfa_ticket SET used_at = now() WHERE user_id = $1::uuid AND used_at IS NULL`,
		userID); err != nil {
		return fmt.Errorf("disable mfa: burn tickets: %w", err)
	}

	if err := insertAudit(ctx, tx, actor, "mfa.disabled", userID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("disable mfa: commit: %w", err)
	}
	return nil
}
