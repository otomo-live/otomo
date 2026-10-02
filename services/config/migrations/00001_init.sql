-- +goose Up
-- The M1 config schema, as design/02-config.md §3 specifies it, with one addition
-- recorded here rather than made silently: the two indexes at the end. §3 lists no
-- indexes, but §5's two history endpoints — a channel's release history and the
-- paginated audit log — both read newest-first, and neither is served by a primary
-- key alone.

-- Schema definitions per namespace.
CREATE TABLE config_namespace (
    name        text PRIMARY KEY,
    audience    text NOT NULL CHECK (audience IN ('client', 'server')),
    description text,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- One row per schema version; a namespace's schema is never edited in place.
CREATE TABLE config_schema (
    namespace      text NOT NULL REFERENCES config_namespace (name),
    schema_version int  NOT NULL,
    body           jsonb NOT NULL,
    created_by     text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace, schema_version)
);

-- One working draft per namespace. revision is the optimistic-locking token: a save
-- states the revision it read, and an UPDATE matching no row is a 409.
CREATE TABLE config_draft (
    namespace    text PRIMARY KEY REFERENCES config_namespace (name),
    body         jsonb NOT NULL,
    base_version int,
    revision     int  NOT NULL DEFAULT 1,
    updated_by   text NOT NULL,
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- Immutable snapshots. Nothing updates this table; a row is written once, at the
-- moment a draft becomes a version.
CREATE TABLE config_version (
    namespace      text NOT NULL REFERENCES config_namespace (name),
    version        int  NOT NULL,
    schema_version int  NOT NULL,
    body           jsonb NOT NULL,
    sha256         char(64) NOT NULL,
    message        text NOT NULL,
    created_by     text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace, version)
);

-- Uploaded binary content. sha256 is UNIQUE because content is addressed by it: the
-- same pack uploaded twice is one row and one blob.
CREATE TABLE content_pack (
    pack_id     uuid PRIMARY KEY,
    name        text NOT NULL,
    sha256      char(64) NOT NULL UNIQUE,
    size_bytes  bigint NOT NULL,
    uploaded_by text NOT NULL,
    uploaded_at timestamptz NOT NULL DEFAULT now()
);

-- A release is a full manifest snapshot for a channel. Rows are append-only: a
-- rollback moves a pointer, it does not delete anything.
CREATE TABLE release (
    release_id         bigserial PRIMARY KEY,
    channel            text NOT NULL CHECK (channel IN ('dev', 'staging', 'live')),
    manifest           jsonb NOT NULL,
    manifest_sha256    char(64) NOT NULL,
    min_client_version text NOT NULL,
    message            text NOT NULL,
    created_by         text NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now()
);

-- The current pointer per channel. Exactly one row per channel, which is what makes
-- "SELECT ... FOR UPDATE" on it the publish lock (design/02-config.md §5a).
CREATE TABLE channel_head (
    channel    text PRIMARY KEY CHECK (channel IN ('dev', 'staging', 'live')),
    release_id bigint NOT NULL REFERENCES release (release_id),
    updated_by text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Every mutating action, written in the same transaction as the change it describes.
CREATE TABLE audit_log (
    id         bigserial PRIMARY KEY,
    at         timestamptz NOT NULL DEFAULT now(),
    actor_id   text NOT NULL,
    actor_name text NOT NULL,
    action     text NOT NULL,
    target     text NOT NULL,
    details    jsonb NOT NULL DEFAULT '{}'
);

CREATE INDEX release_channel_release_id_idx ON release (channel, release_id DESC);
CREATE INDEX audit_log_at_idx ON audit_log (at DESC);

-- +goose Down
DROP TABLE audit_log;
DROP TABLE channel_head;
DROP TABLE release;
DROP TABLE content_pack;
DROP TABLE config_version;
DROP TABLE config_draft;
DROP TABLE config_schema;
DROP TABLE config_namespace;
