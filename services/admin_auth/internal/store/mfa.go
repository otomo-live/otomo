// Queries behind the MFA endpoints: the short-lived login tickets, the TOTP replay
// guard, recovery-code consumption, and the enrollment transaction.
//
// The ticket token is a secret, so only its sha256 reaches the database. The replay
// guard and recovery-code use are single conditional UPDATEs, which is what makes the
// one-time property hold under concurrent requests without an advisory lock.

package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Ticket purposes. They are the two actions a half-authenticated client can take.
const (
	MFAPurposeVerify = "verify"
	MFAPurposeEnroll = "enroll"
)

// MFAMaxAttempts is how many wrong codes one ticket tolerates before it is burned.
const MFAMaxAttempts = 5

// Errors the MFA handlers map onto HTTP responses.
var (
	// ErrTicketInvalid reports a ticket that is unknown, expired or already used.
	ErrTicketInvalid = errors.New("mfa ticket is invalid")
	// ErrMFAAlreadyEnabled reports an enrollment attempt for an account that
	// already has a confirmed factor.
	ErrMFAAlreadyEnabled = errors.New("mfa is already enabled")
	// ErrMFANotEnrolling reports confirmation with no pending secret to confirm.
	ErrMFANotEnrolling = errors.New("no pending mfa enrollment")
	// ErrTOTPReplay reports a code whose step is not newer than the stored one.
	ErrTOTPReplay = errors.New("totp code was already used")
	// ErrRecoveryCodeInvalid reports a recovery code that does not exist or was
	// already consumed.
	ErrRecoveryCodeInvalid = errors.New("recovery code is invalid")
)

// MFATicket is one login challenge. Purpose decides which endpoint may consume it.
type MFATicket struct {
	ID             string
	UserID         string
	Purpose        string
	ExpiresAt      time.Time
	UsedAt         *time.Time
	FailedAttempts int
}

// CreateMFATicket mints a ticket for userID, stores its sha256 for ttl and returns the
// opaque value the client presents back. The token is 32 random bytes as unpadded
// base64url, like the refresh token.
func (d *DB) CreateMFATicket(ctx context.Context, userID, purpose string, ttl time.Duration) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("create mfa ticket: read randomness: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	hash := sha256.Sum256([]byte(token))

	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO mfa_ticket (token_hash, user_id, purpose, expires_at)
		 VALUES ($1, $2::uuid, $3, now() + make_interval(secs => $4))`,
		hash[:], userID, purpose, ttl.Seconds()); err != nil {
		return "", fmt.Errorf("create mfa ticket: %w", err)
	}
	return token, nil
}

// MFATicketByHash returns the live ticket behind tokenHash. Unknown, expired and used
// all report ErrTicketInvalid, so a caller cannot use the distinction as an oracle.
func (d *DB) MFATicketByHash(ctx context.Context, tokenHash []byte) (MFATicket, error) {
	var t MFATicket
	err := d.Pool.QueryRow(ctx,
		`SELECT id::text, user_id::text, purpose, expires_at, used_at, failed_attempts
		 FROM mfa_ticket WHERE token_hash = $1`,
		tokenHash).Scan(&t.ID, &t.UserID, &t.Purpose, &t.ExpiresAt, &t.UsedAt, &t.FailedAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return MFATicket{}, ErrTicketInvalid
	}
	if err != nil {
		return MFATicket{}, fmt.Errorf("mfa ticket by hash: %w", err)
	}
	if t.UsedAt != nil || !t.ExpiresAt.After(time.Now()) {
		return MFATicket{}, ErrTicketInvalid
	}
	return t, nil
}

// MarkMFATicketUsed consumes one ticket. It is idempotent: a second call changes
// nothing and reports no error, because the interesting race is already settled.
func (d *DB) MarkMFATicketUsed(ctx context.Context, id string) error {
	tag, err := d.Pool.Exec(ctx,
		`UPDATE mfa_ticket SET used_at = now()
		 WHERE id = $1::uuid AND used_at IS NULL AND expires_at > now()`,
		id)
	if err != nil {
		return fmt.Errorf("mark mfa ticket used: %w", err)
	}
	// Exactly one row or the ticket was consumed (or expired) by a concurrent
	// request: a ticket signs in at most once.
	if tag.RowsAffected() != 1 {
		return ErrTicketInvalid
	}
	return nil
}

// ClaimMFAAttempt atomically spends one of a verify ticket's attempts and returns the
// ticket. It fails with ErrTicketInvalid when the ticket is unknown, of another purpose,
// used, expired or out of attempts. Counting the attempt before any code is checked is
// what bounds guessing: concurrent requests on one ticket each need their own slot, so
// no more than MFAMaxAttempts codes are ever tried against it.
func (d *DB) ClaimMFAAttempt(ctx context.Context, tokenHash []byte, purpose string, maxAttempts int) (MFATicket, error) {
	var t MFATicket
	err := d.Pool.QueryRow(ctx,
		`UPDATE mfa_ticket
		    SET failed_attempts = failed_attempts + 1
		  WHERE token_hash = $1 AND purpose = $2
		    AND used_at IS NULL AND expires_at > now() AND failed_attempts < $3
		  RETURNING id::text, user_id::text, purpose, expires_at, used_at, failed_attempts`,
		tokenHash, purpose, maxAttempts).Scan(&t.ID, &t.UserID, &t.Purpose, &t.ExpiresAt, &t.UsedAt, &t.FailedAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return MFATicket{}, ErrTicketInvalid
	}
	if err != nil {
		return MFATicket{}, fmt.Errorf("claim mfa attempt: %w", err)
	}
	return t, nil
}

// RecordMFATicketFailure counts one wrong code against a ticket and burns it when the
// attempt reaches maxAttempts. The increment and the burn are one UPDATE, so two
// simultaneous wrong codes cannot each read the same failed_attempts.

// MFAUser is one staff_user row as the MFA path needs it, including the sealed TOTP
// secret. It is separate from User so no other read can accidentally project a secret.
type MFAUser struct {
	ID              string
	Name            string
	Email           string
	Roles           []string
	IsRoot          bool
	Status          string
	LockedUntil     *time.Time
	TOTPConfirmedAt *time.Time
	TOTPSecretEnc   []byte
	TOTPLastStep    *int64
}

// MFAUserByID loads one account with its TOTP material, or an error wrapping
// ErrNotFound.
func (d *DB) MFAUserByID(ctx context.Context, id string) (MFAUser, error) {
	var u MFAUser
	err := d.Pool.QueryRow(ctx,
		`SELECT id::text, name, email, roles, is_root, status, locked_until,
		        totp_confirmed_at, totp_secret_enc, totp_last_step
		 FROM staff_user WHERE id = $1::uuid`,
		id).Scan(&u.ID, &u.Name, &u.Email, &u.Roles, &u.IsRoot, &u.Status,
		&u.LockedUntil, &u.TOTPConfirmedAt, &u.TOTPSecretEnc, &u.TOTPLastStep)
	if errors.Is(err, pgx.ErrNoRows) {
		return MFAUser{}, fmt.Errorf("mfa user by id: %w", ErrNotFound)
	}
	if err != nil {
		return MFAUser{}, fmt.Errorf("mfa user by id: %w", err)
	}
	return u, nil
}

// MarkTOTPStep advances userID's replay guard to step, reporting whether the row moved.
// The predicate makes the update atomic: a step already seen leaves zero rows, which
// the caller treats as an invalid (replayed) code.
func (d *DB) MarkTOTPStep(ctx context.Context, userID string, step int64) (bool, error) {
	tag, err := d.Pool.Exec(ctx,
		`UPDATE staff_user SET totp_last_step = $2, updated_at = now()
		 WHERE id = $1::uuid AND (totp_last_step IS NULL OR totp_last_step < $2)`,
		userID, step)
	if err != nil {
		return false, fmt.Errorf("mark totp step: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ConsumeRecoveryCode marks one unused recovery code of userID used, audits
// mfa.recovery_used with how many codes remain, and returns that count. An unknown or
// already-used code reports ErrRecoveryCodeInvalid.
func (d *DB) ConsumeRecoveryCode(ctx context.Context, userID string, codeHash []byte) (int, error) {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("consume recovery code: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		`UPDATE mfa_recovery_code SET used_at = now()
		 WHERE user_id = $1::uuid AND code_hash = $2 AND used_at IS NULL`,
		userID, codeHash)
	if err != nil {
		return 0, fmt.Errorf("consume recovery code: update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return 0, ErrRecoveryCodeInvalid
	}

	var (
		name      string
		remaining int
	)
	if err := tx.QueryRow(ctx,
		`SELECT name FROM staff_user WHERE id = $1::uuid`, userID).Scan(&name); err != nil {
		return 0, fmt.Errorf("consume recovery code: load actor: %w", err)
	}
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM mfa_recovery_code WHERE user_id = $1::uuid AND used_at IS NULL`,
		userID).Scan(&remaining); err != nil {
		return 0, fmt.Errorf("consume recovery code: count remaining: %w", err)
	}
	if err := insertAuditDetails(ctx, tx, Entry{ID: userID, Name: name}, "mfa.recovery_used", userID,
		map[string]any{"remaining": remaining}); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("consume recovery code: commit: %w", err)
	}
	return remaining, nil
}

// EnrollTOTP stores a fresh sealed secret for userID with the factor still
// unconfirmed, replacing any earlier unconfirmed secret. A confirmed factor is a
// conflict: the account must not be able to silently swap its second factor.
func (d *DB) EnrollTOTP(ctx context.Context, userID string, sealed []byte) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("enroll totp: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var confirmed *time.Time
	err = tx.QueryRow(ctx,
		`SELECT totp_confirmed_at FROM staff_user WHERE id = $1::uuid FOR UPDATE`,
		userID).Scan(&confirmed)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("enroll totp: %w", ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("enroll totp: lookup: %w", err)
	}
	if confirmed != nil {
		return ErrMFAAlreadyEnabled
	}

	if _, err := tx.Exec(ctx,
		`UPDATE staff_user
		 SET totp_secret_enc = $2, totp_confirmed_at = NULL, totp_last_step = NULL, updated_at = now()
		 WHERE id = $1::uuid`,
		userID, sealed); err != nil {
		return fmt.Errorf("enroll totp: update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("enroll totp: commit: %w", err)
	}
	return nil
}

// ConfirmTOTP turns a pending secret into an active factor. In one transaction it
// advances the replay guard to step (rejecting a replayed code via ErrTOTPReplay),
// stamps totp_confirmed_at, replaces every stored recovery code with recoveryHashes
// and audits mfa.enroll. A missing pending secret reports ErrMFANotEnrolling.
func (d *DB) ConfirmTOTP(ctx context.Context, userID string, step int64, recoveryHashes [][]byte) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("confirm totp: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		name, email string
		confirmed   *time.Time
		secret      []byte
	)
	err = tx.QueryRow(ctx,
		`SELECT name, email, totp_confirmed_at, totp_secret_enc
		 FROM staff_user WHERE id = $1::uuid FOR UPDATE`,
		userID).Scan(&name, &email, &confirmed, &secret)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("confirm totp: %w", ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("confirm totp: lookup: %w", err)
	}
	if confirmed != nil {
		return ErrMFAAlreadyEnabled
	}
	if secret == nil {
		return ErrMFANotEnrolling
	}

	tag, err := tx.Exec(ctx,
		`UPDATE staff_user
		 SET totp_confirmed_at = now(), totp_last_step = $2, updated_at = now()
		 WHERE id = $1::uuid AND (totp_last_step IS NULL OR totp_last_step < $2)`,
		userID, step)
	if err != nil {
		return fmt.Errorf("confirm totp: confirm: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTOTPReplay
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM mfa_recovery_code WHERE user_id = $1::uuid`, userID); err != nil {
		return fmt.Errorf("confirm totp: clear recovery codes: %w", err)
	}
	for _, hash := range recoveryHashes {
		if _, err := tx.Exec(ctx,
			`INSERT INTO mfa_recovery_code (user_id, code_hash) VALUES ($1::uuid, $2)`,
			userID, hash); err != nil {
			return fmt.Errorf("confirm totp: insert recovery code: %w", err)
		}
	}

	if err := insertAuditDetails(ctx, tx, Entry{ID: userID, Name: name}, "mfa.enroll", userID,
		map[string]any{"email": email}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("confirm totp: commit: %w", err)
	}
	return nil
}
