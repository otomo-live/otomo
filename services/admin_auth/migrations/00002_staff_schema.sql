-- +goose Up
-- The AA-2 staff account schema, in its own migration so a database that already
-- recorded 00001 (shipped as a no-op in AA-1) picks it up. One row per administrator,
-- plus the one-time
-- invite/reset links, refresh-token sessions, MFA recovery codes, the Ed25519
-- signing keys and the audit trail. Everything a staff login needs, and nothing
-- that belongs to another service.

-- One row per staff account. roles is the full permission set, is_root marks the
-- single bootstrap account, and created_by is NULL only for that root row.
CREATE TABLE staff_user (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email               text NOT NULL,
    name                text NOT NULL,
    password_hash       text NOT NULL,
    roles               text[] NOT NULL,
    is_root             boolean NOT NULL DEFAULT false,
    status              text NOT NULL DEFAULT 'active'
                            CHECK (status IN ('active', 'disabled')),
    totp_secret_enc     bytea,
    totp_confirmed_at   timestamptz,
    failed_logins       int NOT NULL DEFAULT 0 CHECK (failed_logins >= 0),
    locked_until        timestamptz,
    password_changed_at timestamptz NOT NULL DEFAULT now(),
    last_login_at       timestamptz,
    created_by          uuid REFERENCES staff_user (id),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CHECK (cardinality(roles) >= 1
           AND roles <@ ARRAY['viewer', 'live_ops', 'admin']::text[]),
    CHECK (NOT is_root OR roles @> ARRAY['admin']::text[])
);

-- Case-insensitive uniqueness: staff log in by email, so A@x and a@x are one account.
CREATE UNIQUE INDEX staff_user_email_lower_key ON staff_user (lower(email));

-- At most one root account, enforced by a unique index over a constant expression
-- restricted to root rows.
CREATE UNIQUE INDEX staff_user_single_root_idx ON staff_user ((true)) WHERE is_root;

-- One-time invite or password-reset link. token_hash is the sha256 of the link
-- token; the token itself is shown once to the admin who created it and is never
-- stored. purpose decides which of role and user_id is set.
CREATE TABLE staff_invite (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash  bytea NOT NULL UNIQUE,
    purpose     text NOT NULL CHECK (purpose IN ('invite', 'reset')),
    email       text NOT NULL,
    name        text NOT NULL,
    role        text CHECK (role IN ('viewer', 'live_ops', 'admin')),
    user_id     uuid REFERENCES staff_user (id) ON DELETE CASCADE,
    created_by  uuid NOT NULL REFERENCES staff_user (id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    used_at     timestamptz,
    revoked_at  timestamptz,
    CHECK (expires_at > created_at),
    CHECK ((purpose = 'invite' AND role IS NOT NULL AND user_id IS NULL)
           OR (purpose = 'reset' AND user_id IS NOT NULL))
);

-- Index for looking up a live link by the email address it was sent to.
CREATE INDEX staff_invite_email_pending_idx ON staff_invite (lower(email))
    WHERE used_at IS NULL AND revoked_at IS NULL;

-- One row per issued refresh token. Rotation replaces a row by setting rotated_at;
-- revoked_at marks a session killed before it expired. family_id ties a rotation
-- chain together so reuse of an old token can revoke the whole family.
CREATE TABLE refresh_session (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    family_id  uuid NOT NULL,
    user_id    uuid NOT NULL REFERENCES staff_user (id) ON DELETE CASCADE,
    token_hash bytea NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    rotated_at timestamptz,
    revoked_at timestamptz,
    ip         text,
    user_agent text
);

-- Revoke a whole rotation family in one statement.
CREATE INDEX refresh_session_family_id_idx ON refresh_session (family_id);

-- A user's live sessions, for "sign out everywhere" and session listings.
CREATE INDEX refresh_session_user_active_idx ON refresh_session (user_id)
    WHERE revoked_at IS NULL;

-- One-time MFA recovery codes per account. Only the hash is stored; the composite
-- primary key also stops the same code being enrolled twice.
CREATE TABLE mfa_recovery_code (
    user_id   uuid NOT NULL REFERENCES staff_user (id) ON DELETE CASCADE,
    code_hash bytea NOT NULL,
    used_at   timestamptz,
    PRIMARY KEY (user_id, code_hash)
);

-- Ed25519 keys whose public halves are served over JWKS. Same shape as
-- services/auth's table so the signing code can be shared; the keys themselves never
-- are — a staff key in the player JWKS would let a staff token pass as a player one.
CREATE TABLE signing_key (
    kid        text PRIMARY KEY,
    public_key bytea NOT NULL,
    active     boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Every mutating action, written in the same transaction as the change it
-- describes. Same shape as the config service's audit_log.
CREATE TABLE audit_log (
    id         bigserial PRIMARY KEY,
    at         timestamptz NOT NULL DEFAULT now(),
    actor_id   text NOT NULL,
    actor_name text NOT NULL,
    action     text NOT NULL,
    target     text NOT NULL,
    details    jsonb NOT NULL DEFAULT '{}'
);

CREATE INDEX audit_log_at_idx ON audit_log (at DESC);

-- +goose Down
DROP TABLE audit_log;
DROP TABLE signing_key;
DROP TABLE mfa_recovery_code;
DROP TABLE refresh_session;
DROP TABLE staff_invite;
DROP TABLE staff_user;
