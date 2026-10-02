// Queries behind the staff user-management API: listing accounts, creating invite and
// password-reset links, revoking a pending link, and the audited account update.
//
// Every write here happens in one transaction together with the audit row that
// describes it. The permission checks that depend on the target's current row happen
// under a FOR UPDATE lock (or, for invite creation, a per-address advisory lock), so a
// concurrent change cannot slip between the check and the write.

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Errors the user-management handlers map onto HTTP responses.
var (
	// ErrEmailExists reports that a staff account already owns the address.
	ErrEmailExists = errors.New("email already belongs to a staff user")
	// ErrInvitePending reports that a live invite already exists for the address.
	ErrInvitePending = errors.New("a pending invite already exists for that email")
	// ErrInviteNotFound reports an invite that is unknown, used, revoked or expired.
	ErrInviteNotFound = errors.New("no such pending invite")
	// ErrRootProtected reports an attempt to change the break-glass root account,
	// which may only be changed through the CLI.
	ErrRootProtected = errors.New("the root account is protected")
	// ErrSelfModification reports an admin trying to change their own account.
	ErrSelfModification = errors.New("an admin cannot modify their own account")
	// ErrInsufficientRole reports an operation only the root account may perform.
	ErrInsufficientRole = errors.New("only the root account may perform this change")
	// ErrMFANotEnrolled reports a reset of an account that has no second factor,
	// pending secret or recovery codes to remove.
	ErrMFANotEnrolled = errors.New("the account has no mfa material to reset")
)

// adminRole is the role that grants access to the user-management API and whose
// grant or removal is reserved to the root account.
const adminRole = "admin"

// HasRole reports whether roles contains role. It is the shared membership test for
// handlers and the admin middleware.
func HasRole(roles []string, role string) bool {
	for _, r := range roles {
		if r == role {
			return true
		}
	}
	return false
}

// AdminUser is one staff_user row as the management API returns it. PasswordHash and
// the TOTP columns are deliberately absent: this type cannot leak a secret.
type AdminUser struct {
	ID          string
	Email       string
	Name        string
	Roles       []string
	IsRoot      bool
	Status      string
	MFAEnrolled bool
	LastLoginAt *time.Time
	CreatedAt   time.Time
}

// Invite is one staff_invite row as the management API lists it. Role is nil for a
// reset link, which has no role.
type Invite struct {
	ID        string
	Purpose   string
	Email     string
	Name      string
	Role      *string
	CreatedBy string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// adminUserColumns is the projection every AdminUser read shares. The TOTP secret is
// never selected; only whether a factor is confirmed, which is a boolean.
const adminUserColumns = `id::text, email, name, roles, is_root, status,
	(totp_confirmed_at IS NOT NULL), last_login_at, created_at`

// scanAdminUser scans the adminUserColumns projection.
func scanAdminUser(row pgx.Row) (AdminUser, error) {
	var u AdminUser
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.Roles, &u.IsRoot, &u.Status, &u.MFAEnrolled, &u.LastLoginAt, &u.CreatedAt)
	return u, err
}

// ListUsers returns every staff account ordered by creation time. No secret column is
// read, so the result cannot carry a password hash or a TOTP seed.
func (d *DB) ListUsers(ctx context.Context) ([]AdminUser, error) {
	rows, err := d.Pool.Query(ctx,
		`SELECT `+adminUserColumns+` FROM staff_user ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list staff users: %w", err)
	}
	defer rows.Close()

	var users []AdminUser
	for rows.Next() {
		u, err := scanAdminUser(rows)
		if err != nil {
			return nil, fmt.Errorf("list staff users: scan: %w", err)
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list staff users: %w", err)
	}
	return users, nil
}

// CreateInviteInput is one invite to create. TokenHash is the sha256 of the link
// token, never the token itself.
type CreateInviteInput struct {
	TokenHash []byte
	Email     string
	Name      string
	Role      string
	TTL       time.Duration
}

// CreateInvite rejects an address that already belongs to a user or already has a
// live invite, then inserts the invite and its user.invite audit row.
//
// The per-address advisory lock serialises concurrent invites for the same email, so
// two requests cannot both pass the pending check and insert.
func (d *DB) CreateInvite(ctx context.Context, in CreateInviteInput, actor Entry) (Invite, error) {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return Invite{}, fmt.Errorf("create invite: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext(lower($1))::bigint)`, in.Email); err != nil {
		return Invite{}, fmt.Errorf("create invite: lock: %w", err)
	}

	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM staff_user WHERE lower(email) = lower($1))`,
		in.Email).Scan(&exists); err != nil {
		return Invite{}, fmt.Errorf("create invite: check user: %w", err)
	}
	if exists {
		return Invite{}, ErrEmailExists
	}
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (
		     SELECT 1 FROM staff_invite
		     WHERE lower(email) = lower($1) AND purpose = 'invite'
		       AND used_at IS NULL AND revoked_at IS NULL AND expires_at > now())`,
		in.Email).Scan(&exists); err != nil {
		return Invite{}, fmt.Errorf("create invite: check pending: %w", err)
	}
	if exists {
		return Invite{}, ErrInvitePending
	}

	var inv Invite
	role := in.Role
	err = tx.QueryRow(ctx,
		`INSERT INTO staff_invite (token_hash, purpose, email, name, role, created_by, expires_at)
		 VALUES ($1, 'invite', $2, $3, $4, $5::uuid, now() + make_interval(secs => $6))
		 RETURNING id::text, created_at, expires_at`,
		in.TokenHash, in.Email, in.Name, in.Role, actor.ID, in.TTL.Seconds(),
	).Scan(&inv.ID, &inv.CreatedAt, &inv.ExpiresAt)
	if err != nil {
		return Invite{}, fmt.Errorf("create invite: insert: %w", err)
	}
	inv.Purpose = "invite"
	inv.Email = in.Email
	inv.Name = in.Name
	inv.Role = &role
	inv.CreatedBy = actor.ID

	if err := insertAuditDetails(ctx, tx, actor, "user.invite", inv.ID,
		map[string]string{"email": in.Email, "role": in.Role}); err != nil {
		return Invite{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Invite{}, fmt.Errorf("create invite: commit: %w", err)
	}
	return inv, nil
}

// CreateResetInput is one password-reset link to create.
type CreateResetInput struct {
	TokenHash []byte
	UserID    string
	ActorRoot bool
	TTL       time.Duration
}

// CreateReset creates a reset link for an existing account. The target is read FOR
// UPDATE so its root flag and roles cannot change under the permission check. Any
// other live reset for the same account is revoked first, so only the newest link
// works.
func (d *DB) CreateReset(ctx context.Context, in CreateResetInput, actor Entry) (Invite, error) {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return Invite{}, fmt.Errorf("create reset: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		email, name string
		roles       []string
		isRoot      bool
	)
	err = tx.QueryRow(ctx,
		`SELECT email, name, roles, is_root FROM staff_user WHERE id = $1::uuid FOR UPDATE`,
		in.UserID).Scan(&email, &name, &roles, &isRoot)
	if errors.Is(err, pgx.ErrNoRows) {
		return Invite{}, fmt.Errorf("create reset: %w", ErrNotFound)
	}
	if err != nil {
		return Invite{}, fmt.Errorf("create reset: lookup: %w", err)
	}
	if isRoot {
		return Invite{}, ErrRootProtected
	}
	if HasRole(roles, adminRole) && !in.ActorRoot {
		return Invite{}, ErrInsufficientRole
	}

	if _, err := tx.Exec(ctx,
		`UPDATE staff_invite SET revoked_at = now()
		 WHERE user_id = $1::uuid AND purpose = 'reset'
		   AND used_at IS NULL AND revoked_at IS NULL`,
		in.UserID); err != nil {
		return Invite{}, fmt.Errorf("create reset: revoke prior: %w", err)
	}

	var inv Invite
	err = tx.QueryRow(ctx,
		`INSERT INTO staff_invite (token_hash, purpose, email, name, user_id, created_by, expires_at)
		 VALUES ($1, 'reset', $2, $3, $4::uuid, $5::uuid, now() + make_interval(secs => $6))
		 RETURNING id::text, created_at, expires_at`,
		in.TokenHash, email, name, in.UserID, actor.ID, in.TTL.Seconds(),
	).Scan(&inv.ID, &inv.CreatedAt, &inv.ExpiresAt)
	if err != nil {
		return Invite{}, fmt.Errorf("create reset: insert: %w", err)
	}
	inv.Purpose = "reset"
	inv.Email = email
	inv.Name = name
	inv.CreatedBy = actor.ID

	if err := insertAuditDetails(ctx, tx, actor, "user.reset_link", inv.ID,
		map[string]string{"email": email}); err != nil {
		return Invite{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Invite{}, fmt.Errorf("create reset: commit: %w", err)
	}
	return inv, nil
}

// ListInvites returns the live invite and reset links, newest last. Tokens and their
// hashes are never selected.
func (d *DB) ListInvites(ctx context.Context) ([]Invite, error) {
	rows, err := d.Pool.Query(ctx,
		`SELECT id::text, purpose, email, name, role, created_by::text, created_at, expires_at
		 FROM staff_invite
		 WHERE used_at IS NULL AND revoked_at IS NULL AND expires_at > now()
		 ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list invites: %w", err)
	}
	defer rows.Close()

	var invites []Invite
	for rows.Next() {
		var inv Invite
		if err := rows.Scan(&inv.ID, &inv.Purpose, &inv.Email, &inv.Name, &inv.Role, &inv.CreatedBy, &inv.CreatedAt, &inv.ExpiresAt); err != nil {
			return nil, fmt.Errorf("list invites: scan: %w", err)
		}
		invites = append(invites, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list invites: %w", err)
	}
	return invites, nil
}

// RevokeInvite stamps revoked_at on a live invite and writes its audit row. An
// unknown, used, revoked or expired invite reports ErrInviteNotFound.
func (d *DB) RevokeInvite(ctx context.Context, id string, actor Entry) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("revoke invite: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var email string
	err = tx.QueryRow(ctx,
		`UPDATE staff_invite SET revoked_at = now()
		 WHERE id = $1::uuid AND used_at IS NULL AND revoked_at IS NULL AND expires_at > now()
		 RETURNING email`,
		id).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInviteNotFound
	}
	if err != nil {
		return fmt.Errorf("revoke invite: update: %w", err)
	}

	if err := insertAuditDetails(ctx, tx, actor, "invite.revoke", id,
		map[string]string{"email": email}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("revoke invite: commit: %w", err)
	}
	return nil
}

// UpdateUserInput is one PATCH. HasRoles and HasStatus distinguish an omitted field
// from an explicitly empty one; an admin-status change is judged against the roles
// the target holds now, under the row lock.
type UpdateUserInput struct {
	ID        string
	ActorID   string
	ActorRoot bool
	Roles     []string
	HasRoles  bool
	Status    string
	HasStatus bool
}

// UpdateUser applies a roles and/or status change and writes the before/after
// user.update audit row in the same transaction. It enforces root protection, the
// self-modification ban and the root-only admin rules; disabling an account also
// revokes its live refresh sessions here, so it cannot refresh its way back in.
func (d *DB) UpdateUser(ctx context.Context, in UpdateUserInput, actor Entry) (AdminUser, error) {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return AdminUser{}, fmt.Errorf("update user: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		oldRoles  []string
		oldStatus string
		isRoot    bool
	)
	err = tx.QueryRow(ctx,
		`SELECT roles, status, is_root FROM staff_user WHERE id = $1::uuid FOR UPDATE`,
		in.ID).Scan(&oldRoles, &oldStatus, &isRoot)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminUser{}, fmt.Errorf("update user: %w", ErrNotFound)
	}
	if err != nil {
		return AdminUser{}, fmt.Errorf("update user: lookup: %w", err)
	}
	if isRoot {
		return AdminUser{}, ErrRootProtected
	}
	if in.ID == in.ActorID {
		return AdminUser{}, ErrSelfModification
	}

	newRoles := oldRoles
	if in.HasRoles {
		// Granting or removing admin is root-only, whether or not the request
		// touches any other role.
		if HasRole(oldRoles, adminRole) != HasRole(in.Roles, adminRole) && !in.ActorRoot {
			return AdminUser{}, ErrInsufficientRole
		}
		newRoles = in.Roles
	}
	newStatus := oldStatus
	if in.HasStatus {
		newStatus = in.Status
		// Changing the status of an admin is root-only.
		if newStatus != oldStatus && HasRole(oldRoles, adminRole) && !in.ActorRoot {
			return AdminUser{}, ErrInsufficientRole
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE staff_user SET roles = $2, status = $3, updated_at = now() WHERE id = $1::uuid`,
		in.ID, newRoles, newStatus); err != nil {
		return AdminUser{}, fmt.Errorf("update user: update: %w", err)
	}

	if in.HasStatus && newStatus == "disabled" && oldStatus != "disabled" {
		if _, err := tx.Exec(ctx,
			`UPDATE refresh_session SET revoked_at = now() WHERE user_id = $1::uuid AND revoked_at IS NULL`,
			in.ID); err != nil {
			return AdminUser{}, fmt.Errorf("update user: revoke sessions: %w", err)
		}
	}

	details := map[string]any{
		"before": map[string]any{"roles": oldRoles, "status": oldStatus},
		"after":  map[string]any{"roles": newRoles, "status": newStatus},
	}
	if err := insertAuditDetails(ctx, tx, actor, "user.update", in.ID, details); err != nil {
		return AdminUser{}, err
	}

	u, err := scanAdminUser(tx.QueryRow(ctx,
		`SELECT `+adminUserColumns+` FROM staff_user WHERE id = $1::uuid`, in.ID))
	if err != nil {
		return AdminUser{}, fmt.Errorf("update user: reload: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AdminUser{}, fmt.Errorf("update user: commit: %w", err)
	}
	return u, nil
}

// ResetMFAResult is what one admin-initiated MFA reset did, for the audit details
// and the response body.
type ResetMFAResult struct {
	// User is the target row after the reset, so the handler can return the same
	// admin user JSON every other user-management route returns.
	User AdminUser
	// HadFactor is true when the target had a confirmed second factor before the
	// reset. A pending secret or recovery codes alone leave it false.
	HadFactor bool
	// RecoveryCodesRemoved is how many recovery-code rows were deleted.
	RecoveryCodesRemoved int
	// SessionsRevoked is how many live refresh sessions were killed.
	SessionsRevoked int
}

// ResetMFA clears the target's second factor, recovery codes and MFA tickets,
// revokes its live refresh sessions, and writes the mfa.reset audit row — all in one
// transaction after locking the user row FOR UPDATE. That lock is what makes a
// second concurrent reset see the cleared row and report ErrMFANotEnrolled instead of
// doing the work (and writing a second audit row) twice.
//
// The rule checks mirror UpdateUser: an unknown target, the protected root account,
// self-modification and the root-only admin rule are all diagnosed under the lock.
func (d *DB) ResetMFA(ctx context.Context, userID string, actor Entry) (ResetMFAResult, error) {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return ResetMFAResult{}, fmt.Errorf("reset mfa: begin: %w", err)
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
		return ResetMFAResult{}, fmt.Errorf("reset mfa: %w", ErrNotFound)
	}
	if err != nil {
		return ResetMFAResult{}, fmt.Errorf("reset mfa: lookup: %w", err)
	}
	if isRoot {
		return ResetMFAResult{}, ErrRootProtected
	}
	if userID == actor.ID {
		return ResetMFAResult{}, ErrSelfModification
	}
	// The caller the middleware put on the context is only an id/name pair, so the
	// root flag the admin rule needs is read from the actor's own row in this same
	// transaction.
	var actorRoot bool
	if err := tx.QueryRow(ctx,
		`SELECT is_root FROM staff_user WHERE id = $1::uuid`, actor.ID).Scan(&actorRoot); err != nil {
		return ResetMFAResult{}, fmt.Errorf("reset mfa: load actor: %w", err)
	}
	if HasRole(roles, adminRole) && !actorRoot {
		return ResetMFAResult{}, ErrInsufficientRole
	}

	var recoveryCount int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM mfa_recovery_code WHERE user_id = $1::uuid`, userID).Scan(&recoveryCount); err != nil {
		return ResetMFAResult{}, fmt.Errorf("reset mfa: count recovery codes: %w", err)
	}
	if confirmedAt == nil && secret == nil && recoveryCount == 0 {
		return ResetMFAResult{}, ErrMFANotEnrolled
	}
	hadFactor := confirmedAt != nil

	if _, err := tx.Exec(ctx,
		`UPDATE staff_user
		 SET totp_secret_enc = NULL, totp_confirmed_at = NULL, totp_last_step = NULL, updated_at = now()
		 WHERE id = $1::uuid`,
		userID); err != nil {
		return ResetMFAResult{}, fmt.Errorf("reset mfa: clear factor: %w", err)
	}

	tag, err := tx.Exec(ctx,
		`DELETE FROM mfa_recovery_code WHERE user_id = $1::uuid`, userID)
	if err != nil {
		return ResetMFAResult{}, fmt.Errorf("reset mfa: delete recovery codes: %w", err)
	}
	removed := int(tag.RowsAffected())

	if _, err := tx.Exec(ctx,
		`UPDATE mfa_ticket SET used_at = now() WHERE user_id = $1::uuid AND used_at IS NULL`,
		userID); err != nil {
		return ResetMFAResult{}, fmt.Errorf("reset mfa: burn tickets: %w", err)
	}

	tag, err = tx.Exec(ctx,
		`UPDATE refresh_session SET revoked_at = now() WHERE user_id = $1::uuid AND revoked_at IS NULL`,
		userID)
	if err != nil {
		return ResetMFAResult{}, fmt.Errorf("reset mfa: revoke sessions: %w", err)
	}
	revoked := int(tag.RowsAffected())

	if err := insertAuditDetails(ctx, tx, actor, "mfa.reset", userID, map[string]any{
		"had_factor":             hadFactor,
		"recovery_codes_removed": removed,
		"sessions_revoked":       revoked,
	}); err != nil {
		return ResetMFAResult{}, err
	}

	u, err := scanAdminUser(tx.QueryRow(ctx,
		`SELECT `+adminUserColumns+` FROM staff_user WHERE id = $1::uuid`, userID))
	if err != nil {
		return ResetMFAResult{}, fmt.Errorf("reset mfa: reload: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ResetMFAResult{}, fmt.Errorf("reset mfa: commit: %w", err)
	}
	return ResetMFAResult{
		User:                 u,
		HadFactor:            hadFactor,
		RecoveryCodesRemoved: removed,
		SessionsRevoked:      revoked,
	}, nil
}
