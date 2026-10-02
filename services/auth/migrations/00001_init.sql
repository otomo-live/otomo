-- +goose Up
CREATE TABLE account (
    account_id uuid PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE identity_binding (
    method      text NOT NULL,
    external_id text NOT NULL,
    account_id  uuid NOT NULL REFERENCES account (account_id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (method, external_id)
);

CREATE TABLE signing_key (
    kid        text PRIMARY KEY,
    public_key bytea NOT NULL,
    active     boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE refresh_token (
    token_id   uuid PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES account (account_id),
    family_id  uuid NOT NULL,
    token_hash bytea NOT NULL,
    revoked    boolean NOT NULL DEFAULT false,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE refresh_token;
DROP TABLE identity_binding;
DROP TABLE signing_key;
DROP TABLE account;
