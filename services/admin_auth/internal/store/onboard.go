// The onboarding queries behind POST /admin-auth/onboard/lookup and
// POST /admin-auth/onboard.
//
// A link token is a secret; only its sha256 reaches here. The lookup is a plain
// non-locking read. The redemption takes the link row FOR UPDATE by that hash, so
// simultaneous redemptions of one link serialise: the first stamps used_at and
// commits, and every later transaction re-evaluates the "still pending" predicate
// against the updated row and finds nothing.
//
// An invite that lost the email race still burns the link before reporting
// ErrEmailExists, because a link that could be replayed after a conflict is a link an
// attacker gets a second attempt with. A reset that targets a missing or disabled
// account reports ErrInviteNotFound and leaves the link pending, because nothing about
// that account can be changed.

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

// OnboardLink is a pending invite or reset link as the lookup endpoint returns it.
// Role is nil for a reset link.
type OnboardLink struct {
	Purpose   string
	Email     string
	Name      string
	Role      *string
	ExpiresAt time.Time
}

// RedeemLinkInput is one onboarding redemption. PasswordHash is the argon2id PHC
// string; the policy checks happen before this is called.
type RedeemLinkInput struct {
	TokenHash    []byte
	PasswordHash string
}

// RedeemedUser is the account a successful redemption created or updated. It carries
// the same factor state the login path reads, so the handler can apply the identical
// post-password MFA policy without a second, racy lookup.
type RedeemedUser struct {
	ID              string
	Name            string
	Email           string
	Roles           []string
	IsRoot          bool
	TOTPConfirmedAt *time.Time
}

// LookupLink returns the pending invite or reset behind tokenHash, or an error
// wrapping ErrInviteNotFound when no such link exists or it is used, revoked or
// expired.
func (d *DB) LookupLink(ctx context.Context, tokenHash []byte) (OnboardLink, error) {
	var l OnboardLink
	err := d.Pool.QueryRow(ctx,
		`SELECT purpose, email, name, role, expires_at
		 FROM staff_invite
		 WHERE token_hash = $1 AND used_at IS NULL AND revoked_at IS NULL AND expires_at > now()`,
		tokenHash).Scan(&l.Purpose, &l.Email, &l.Name, &l.Role, &l.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return OnboardLink{}, fmt.Errorf("lookup link: %w", ErrInviteNotFound)
	}
	if err != nil {
		return OnboardLink{}, fmt.Errorf("lookup link: %w", err)
	}
	return l, nil
}

// RedeemLink consumes one link in a single transaction.
//
// Invite: create the staff_user with the invited role and created_by from the link,
// then audit user.onboard with the new account as the actor. If the address became a
// user meanwhile the link is still marked used and ErrEmailExists is returned.
//
// Reset: re-read the target under FOR UPDATE, refuse a missing or non-active account,
// replace the hash and reset the lockout, revoke every live refresh session, then
// audit user.password_reset with the target as the actor.
//
// In both cases used_at is stamped in the same transaction as the change, so a crash
// cannot consume a link without recording what it did.
func (d *DB) RedeemLink(ctx context.Context, in RedeemLinkInput) (RedeemedUser, error) {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return RedeemedUser{}, fmt.Errorf("redeem link: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		id, purpose, email, name, createdBy string
		role, userID                        *string
	)
	err = tx.QueryRow(ctx,
		`SELECT id::text, purpose, email, name, role, user_id::text, created_by::text
		 FROM staff_invite
		 WHERE token_hash = $1 AND used_at IS NULL AND revoked_at IS NULL AND expires_at > now()
		 FOR UPDATE`,
		in.TokenHash).Scan(&id, &purpose, &email, &name, &role, &userID, &createdBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return RedeemedUser{}, fmt.Errorf("redeem link: %w", ErrInviteNotFound)
	}
	if err != nil {
		return RedeemedUser{}, fmt.Errorf("redeem link: lookup: %w", err)
	}

	switch purpose {
	case "invite":
		if role == nil {
			return RedeemedUser{}, fmt.Errorf("redeem link: invite %s has no role", id)
		}

		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM staff_user WHERE lower(email) = lower($1))`,
			email).Scan(&exists); err != nil {
			return RedeemedUser{}, fmt.Errorf("redeem link: check email: %w", err)
		}
		if exists {
			// Burn the link in the same transaction that reports the conflict,
			// so a 409 can never be followed by a successful replay.
			if err := markLinkUsed(ctx, tx, id); err != nil {
				return RedeemedUser{}, err
			}
			if err := tx.Commit(ctx); err != nil {
				return RedeemedUser{}, fmt.Errorf("redeem link: commit conflict: %w", err)
			}
			return RedeemedUser{}, ErrEmailExists
		}

		u := RedeemedUser{Email: strings.ToLower(email)}
		err = tx.QueryRow(ctx,
			`INSERT INTO staff_user (email, name, password_hash, roles, created_by)
			 VALUES ($1, $2, $3, $4, $5::uuid)
			 RETURNING id::text, name, email, roles, is_root, totp_confirmed_at`,
			u.Email, name, in.PasswordHash, []string{*role}, createdBy,
		).Scan(&u.ID, &u.Name, &u.Email, &u.Roles, &u.IsRoot, &u.TOTPConfirmedAt)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				// A concurrent path created the account between the check and the
				// insert. The aborted transaction cannot write, so burn the link
				// from a fresh one and report the same conflict.
				_ = tx.Rollback(ctx)
				if burnErr := d.burnLink(ctx, in.TokenHash); burnErr != nil {
					return RedeemedUser{}, burnErr
				}
				return RedeemedUser{}, ErrEmailExists
			}
			return RedeemedUser{}, fmt.Errorf("redeem link: insert user: %w", err)
		}
		if err := insertAuditDetails(ctx, tx, Entry{ID: u.ID, Name: u.Name}, "user.onboard", u.ID,
			map[string]string{"email": u.Email, "role": *role}); err != nil {
			return RedeemedUser{}, err
		}
		if err := markLinkUsed(ctx, tx, id); err != nil {
			return RedeemedUser{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return RedeemedUser{}, fmt.Errorf("redeem link: commit: %w", err)
		}
		return u, nil

	case "reset":
		if userID == nil {
			return RedeemedUser{}, fmt.Errorf("redeem link: reset %s has no user", id)
		}

		u := RedeemedUser{ID: *userID}
		var status string
		err = tx.QueryRow(ctx,
			`SELECT name, email, roles, status, is_root, totp_confirmed_at
			 FROM staff_user WHERE id = $1::uuid FOR UPDATE`,
			u.ID).Scan(&u.Name, &u.Email, &u.Roles, &status, &u.IsRoot, &u.TOTPConfirmedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return RedeemedUser{}, fmt.Errorf("redeem link: target: %w", ErrInviteNotFound)
		}
		if err != nil {
			return RedeemedUser{}, fmt.Errorf("redeem link: target: %w", err)
		}
		if status != "active" {
			return RedeemedUser{}, fmt.Errorf("redeem link: target is %s: %w", status, ErrInviteNotFound)
		}

		if _, err := tx.Exec(ctx,
			`UPDATE staff_user
			 SET password_hash = $2, password_changed_at = now(),
			     failed_logins = 0, locked_until = NULL, updated_at = now()
			 WHERE id = $1::uuid`,
			u.ID, in.PasswordHash); err != nil {
			return RedeemedUser{}, fmt.Errorf("redeem link: set password: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE refresh_session SET revoked_at = now()
			 WHERE user_id = $1::uuid AND revoked_at IS NULL`,
			u.ID); err != nil {
			return RedeemedUser{}, fmt.Errorf("redeem link: revoke sessions: %w", err)
		}
		if err := insertAuditDetails(ctx, tx, Entry{ID: u.ID, Name: u.Name}, "user.password_reset", u.ID,
			map[string]string{"email": u.Email}); err != nil {
			return RedeemedUser{}, err
		}
		if err := markLinkUsed(ctx, tx, id); err != nil {
			return RedeemedUser{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return RedeemedUser{}, fmt.Errorf("redeem link: commit: %w", err)
		}
		return u, nil
	}

	return RedeemedUser{}, fmt.Errorf("redeem link: unknown purpose %q", purpose)
}

// markLinkUsed stamps used_at in the caller's transaction. The row is already locked
// by the redemption, so this cannot race another redemption.
func markLinkUsed(ctx context.Context, tx pgx.Tx, id string) error {
	if _, err := tx.Exec(ctx,
		`UPDATE staff_invite SET used_at = now() WHERE id = $1::uuid`, id); err != nil {
		return fmt.Errorf("redeem link: mark used: %w", err)
	}
	return nil
}

// burnLink marks the link used in its own transaction. It is the recovery path for a
// unique-index conflict, where the original transaction has already aborted.
func (d *DB) burnLink(ctx context.Context, tokenHash []byte) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("burn link: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`UPDATE staff_invite SET used_at = now() WHERE token_hash = $1 AND used_at IS NULL`,
		tokenHash); err != nil {
		return fmt.Errorf("burn link: update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("burn link: commit: %w", err)
	}
	return nil
}
