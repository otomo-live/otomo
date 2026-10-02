-- +goose Up
-- Every channel gets a head before the first publish, so Patch can answer for any
-- channel from the moment the stack comes up rather than 404ing until a staff member
-- happens to publish something. Each bootstrap release carries an empty client
-- manifest: format 1, no config entries, no packs.
--
-- The manifest text is written in sorted key order with no insignificant whitespace,
-- which is the canonical form §3's hashing rule produces, so manifest_sha256 here is
-- the same hash the publish path would compute for the same document. release_id has
-- to come from the sequence before the INSERT, because the manifest states it — hence
-- nextval rather than the column default.

-- +goose StatementBegin
DO $$
DECLARE
    ch   text;
    id   bigint;
    body text;
BEGIN
    FOREACH ch IN ARRAY ARRAY['dev', 'staging', 'live'] LOOP
        id := nextval(pg_get_serial_sequence('release', 'release_id'));
        body := format(
            '{"channel":"%s","config":{},"format":1,"min_client_version":"0.0.0","packs":[],"release_id":%s}',
            ch, id);

        INSERT INTO release (release_id, channel, manifest, manifest_sha256,
                             min_client_version, message, created_by)
        VALUES (id, ch, body::jsonb, encode(sha256(body::bytea), 'hex'),
                '0.0.0', 'bootstrap release; created by migration 00002', 'migration');

        INSERT INTO channel_head (channel, release_id, updated_by)
        VALUES (ch, id, 'migration');
    END LOOP;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- channel_head rows must go first: they reference the releases being deleted.
DELETE FROM channel_head WHERE updated_by = 'migration';
DELETE FROM release
 WHERE created_by = 'migration'
   AND message = 'bootstrap release; created by migration 00002';
