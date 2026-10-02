-- +goose Up
-- Releases now carry a second manifest for server-audience namespaces. The client
-- manifest keeps the exact shape and hash rule it had; the server manifest is built by
-- the same code path with the same document format, holds only the server config and
-- always has an empty packs array.
--
-- server_manifest is nullable so rows written before this migration stay valid. NULL
-- means "released before server manifests existed; treat as an empty server manifest".
-- The CHECK keeps the two new columns in lockstep: a manifest without its hash, or a
-- hash without its manifest, would let Patch serve bytes it cannot verify.
ALTER TABLE release
    ADD COLUMN server_manifest        jsonb,
    ADD COLUMN server_manifest_sha256 char(64),
    ADD CONSTRAINT release_server_manifest_pair_check
        CHECK ((server_manifest IS NULL) = (server_manifest_sha256 IS NULL));

-- +goose Down
ALTER TABLE release
    DROP CONSTRAINT release_server_manifest_pair_check,
    DROP COLUMN server_manifest_sha256,
    DROP COLUMN server_manifest;
