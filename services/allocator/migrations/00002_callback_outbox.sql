-- Session callback: the allocation outbox gets a retry schedule.
--
-- callback_pending already marks an ended allocation Session has not been told about.
-- These two columns turn that flag into a queue. callback_attempts counts how many
-- deliveries have failed, and callback_next_at is when the row next becomes due; a NULL
-- callback_next_at means "due now", which is how every end path (Ended, the reaper's
-- reap, the restart path, and reservation expiry) leaves a freshly ended allocation.
--
-- The partial index changes shape with the drain query. The drain claims rows by
-- (callback_pending, callback_next_at <= now()) in ended_at order, so the index leads
-- with callback_next_at; 00001 led with ended_at, which no longer serves the claim.

-- +goose Up
ALTER TABLE allocation
    ADD COLUMN callback_attempts smallint NOT NULL DEFAULT 0,
    ADD COLUMN callback_next_at timestamptz;
DROP INDEX allocation_callback_pending_idx;
CREATE INDEX allocation_callback_pending_idx ON allocation (callback_next_at) WHERE callback_pending;

-- +goose Down
DROP INDEX allocation_callback_pending_idx;
CREATE INDEX allocation_callback_pending_idx ON allocation (ended_at) WHERE callback_pending;
ALTER TABLE allocation
    DROP COLUMN callback_next_at,
    DROP COLUMN callback_attempts;
