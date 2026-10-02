-- +goose Up
-- AA-2 creates the staff account schema here. This migration is intentionally a
-- no-op: it exists so the migration tooling, its embedded file set and its version
-- bookkeeping are exercised from the very first ticket.

-- +goose Down
-- Nothing to undo yet; AA-2 fills in the Down side alongside the schema.
