// Queries for refresh_token: issue, rotate on use, and revoke a whole family.
//
// A refresh token is opaque and stored only as its SHA-256 hash. Every token issued
// by one login belongs to one family; each successful refresh exchanges the presented
// token for a successor in the same family. Presenting a token that was already
// exchanged means two parties hold the same credential, so the whole family is
// revoked and the player must log in again.

package store

import (
	"context"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
)

// RotateOutcome is what RotateRefreshToken decided about one presented token.
type RotateOutcome int

const (
	// RotateInvalid: no row matches, or the row has expired. Nothing was written.
	RotateInvalid RotateOutcome = iota
	// RotateReused: the token had already been exchanged. The family is now revoked.
	RotateReused
	// RotateRevoked: the token was revoked without being exchanged (logout, or its
	// family was revoked earlier). Its family is revoked, which is already the case.
	RotateRevoked
	// RotateRotated: the normal path. The presented token is spent and its successor
	// was inserted with a fresh expiry.
	RotateRotated
)

// RotateInput is one refresh. Both hashes are SHA-256 digests, never raw tokens.
type RotateInput struct {
	TokenHash    []byte
	NewTokenHash []byte
	TTL          time.Duration // the successor's lifetime, from Now: expiry slides on every use
	Now          time.Time
}

// RotateResult carries the outcome and, when a row was found, whose it was.
type RotateResult struct {
	Outcome   RotateOutcome
	AccountID string
	FamilyID  string
}

// InsertRefreshToken stores the first token of a new family and returns the family ID.
func (d *DB) InsertRefreshToken(ctx context.Context, accountID string, tokenHash []byte, expiresAt time.Time) (string, error) {
	familyID := uuid.New().String()
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO refresh_token (token_id, account_id, family_id, token_hash, expires_at)
		 VALUES ($1, $2, $3, $4, $5)`,
		uuid.New().String(), accountID, familyID, tokenHash, expiresAt); err != nil {
		return "", fmt.Errorf("insert refresh token: %w", err)
	}
	return familyID, nil
}

// RotateRefreshToken exchanges one refresh token for its successor, atomically.
//
// The row is read FOR UPDATE, so concurrent refreshes of the same token serialise:
// the first rotates it, and every later one then reads a spent token and revokes the
// family — including the successor the first one just issued. That is deliberate. The
// contract has no grace window, because a client that refreshes twice with one token
// is indistinguishable from a thief racing the owner.
//
// The checks run in this order: an unknown token is invalid; a spent or revoked token
// revokes the family whatever its expiry; only then is an expired token refused.
func (d *DB) RotateRefreshToken(ctx context.Context, in RotateInput) (RotateResult, error) {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return RotateResult{}, fmt.Errorf("rotate refresh token: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		tokenID   string
		res       RotateResult
		revoked   bool
		rotatedAt *time.Time
		expiresAt time.Time
	)
	err = tx.QueryRow(ctx,
		`SELECT token_id::text, account_id::text, family_id::text, revoked, rotated_at, expires_at
		 FROM refresh_token WHERE token_hash = $1
		 FOR UPDATE`,
		in.TokenHash).Scan(&tokenID, &res.AccountID, &res.FamilyID, &revoked, &rotatedAt, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return RotateResult{Outcome: RotateInvalid}, nil
	}
	if err != nil {
		return RotateResult{}, fmt.Errorf("rotate refresh token: lookup: %w", err)
	}

	if revoked {
		if err := revokeFamily(ctx, tx, res.FamilyID); err != nil {
			return RotateResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return RotateResult{}, fmt.Errorf("rotate refresh token: commit family revocation: %w", err)
		}
		res.Outcome = RotateRevoked
		if rotatedAt != nil {
			res.Outcome = RotateReused
		}
		return res, nil
	}

	if !expiresAt.After(in.Now) {
		res.Outcome = RotateInvalid
		return res, nil
	}

	// The spend is conditional as well as locked (design/07 §6): if anything spent or
	// revoked this row since it was read, no row is affected and this presentation is
	// reuse. With the FOR UPDATE above that cannot happen, but the invariant "a token is
	// exchanged at most once" should not rest on a single clause.
	tag, err := tx.Exec(ctx,
		`UPDATE refresh_token SET revoked = true, rotated_at = $2 WHERE token_id = $1 AND NOT revoked`,
		tokenID, in.Now)
	if err != nil {
		return RotateResult{}, fmt.Errorf("rotate refresh token: spend: %w", err)
	}
	if tag.RowsAffected() != 1 {
		if err := revokeFamily(ctx, tx, res.FamilyID); err != nil {
			return RotateResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return RotateResult{}, fmt.Errorf("rotate refresh token: commit family revocation: %w", err)
		}
		res.Outcome = RotateReused
		return res, nil
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO refresh_token (token_id, account_id, family_id, token_hash, expires_at)
		 VALUES ($1, $2, $3, $4, $5)`,
		uuid.New().String(), res.AccountID, res.FamilyID, in.NewTokenHash, in.Now.Add(in.TTL)); err != nil {
		return RotateResult{}, fmt.Errorf("rotate refresh token: insert successor: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return RotateResult{}, fmt.Errorf("rotate refresh token: commit: %w", err)
	}
	res.Outcome = RotateRotated
	return res, nil
}

// RevokeRefreshFamily revokes every live token in the family of the token with this
// hash. A hash that matches no row is not an error: logout is idempotent.
func (d *DB) RevokeRefreshFamily(ctx context.Context, tokenHash []byte) error {
	if _, err := d.Pool.Exec(ctx,
		`UPDATE refresh_token SET revoked = true
		 WHERE family_id = (SELECT family_id FROM refresh_token WHERE token_hash = $1)
		   AND NOT revoked`,
		tokenHash); err != nil {
		return fmt.Errorf("revoke refresh family: %w", err)
	}
	return nil
}

// revokeFamily marks every live token in the family revoked, inside tx.
func revokeFamily(ctx context.Context, tx pgx.Tx, familyID string) error {
	if _, err := tx.Exec(ctx,
		`UPDATE refresh_token SET revoked = true WHERE family_id = $1 AND NOT revoked`,
		familyID); err != nil {
		return fmt.Errorf("revoke refresh family %s: %w", familyID, err)
	}
	return nil
}
