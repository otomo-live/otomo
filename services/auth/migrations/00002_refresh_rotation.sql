-- AUTH-5: refresh-token rotation and family revocation.
--
-- token_hash is how every refresh and logout finds its row, so it gets a unique
-- index: a lookup must hit exactly one row, and a duplicate hash would mean two
-- sessions share one credential. family_id is what reuse detection and logout revoke
-- in one statement. rotated_at tells a replayed, already-exchanged token (theft) from
-- one that was merely revoked; both are refused, but only the first is a security
-- signal worth logging as reuse.

-- +goose Up
CREATE UNIQUE INDEX refresh_token_token_hash_key ON refresh_token (token_hash);
CREATE INDEX refresh_token_family_id_idx ON refresh_token (family_id);
ALTER TABLE refresh_token ADD COLUMN rotated_at timestamptz;

-- +goose Down
ALTER TABLE refresh_token DROP COLUMN rotated_at;
DROP INDEX refresh_token_family_id_idx;
DROP INDEX refresh_token_token_hash_key;
