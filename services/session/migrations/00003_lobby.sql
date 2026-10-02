-- +goose Up
-- LB-2: the party is the lobby (design/14-launch-handoff.md §2.1, verbatim).
--
-- state is the lobby state machine: forming (the leader may change settings and the
-- members may ready up), launching (Session has asked the Allocator for a game server)
-- and in_game (the match runs; allocation_id names it). settings is a flat object of
-- string values, one per session.rules lobby setting. ready is per member and is cleared
-- whenever the settings change.
ALTER TABLE party
  ADD COLUMN state text NOT NULL DEFAULT 'forming'
      CHECK (state IN ('forming','launching','in_game')),
  ADD COLUMN settings jsonb NOT NULL DEFAULT '{}',
  ADD COLUMN state_changed_at timestamptz NOT NULL DEFAULT now(),
  ADD COLUMN allocation_id uuid;                    -- set in in_game only
ALTER TABLE party_member ADD COLUMN ready boolean NOT NULL DEFAULT false;

-- The launch sweep and the repair poll (doc 14 §6) scan only parties away from forming.
CREATE INDEX party_state_idx ON party (state, state_changed_at) WHERE state <> 'forming';

-- +goose Down
DROP INDEX party_state_idx;
ALTER TABLE party_member DROP COLUMN ready;
ALTER TABLE party
  DROP COLUMN allocation_id,
  DROP COLUMN state_changed_at,
  DROP COLUMN settings,
  DROP COLUMN state;
