-- +goose Up
-- The AA-5 MFA schema. TOTP secrets live in staff_user.totp_secret_enc (added in
-- 00002) and mfa_recovery_code already exists; this migration adds the per-user
-- replay guard and the short-lived login challenge tickets.
--
-- totp_last_step is the highest TOTP step a code for this account has ever been
-- accepted at. Because the 30-second step is monotonic, storing it lets one atomic
-- UPDATE reject a replayed code without a separate nonce table.
ALTER TABLE staff_user ADD COLUMN totp_last_step bigint;

-- One login challenge ticket, minted after the password is verified and consumed by
-- /admin-auth/mfa/verify or /admin-auth/mfa/confirm. Only the sha256 of the ticket is
-- stored, so a database reader cannot replay one; used_at and expires_at bound its
-- life, and failed_attempts burns it after repeated wrong codes.
CREATE TABLE mfa_ticket (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash      bytea NOT NULL UNIQUE,
    user_id         uuid NOT NULL REFERENCES staff_user (id) ON DELETE CASCADE,
    purpose         text NOT NULL CHECK (purpose IN ('verify', 'enroll')),
    expires_at      timestamptz NOT NULL,
    used_at         timestamptz,
    failed_attempts int NOT NULL DEFAULT 0 CHECK (failed_attempts >= 0),
    created_at      timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at > created_at)
);

-- Look up and clean up a user's tickets.
CREATE INDEX mfa_ticket_user_id_idx ON mfa_ticket (user_id);

-- +goose Down
DROP TABLE mfa_ticket;
ALTER TABLE staff_user DROP COLUMN totp_last_step;
