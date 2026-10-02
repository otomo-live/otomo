-- Allocator skeleton: the game-server pool and the allocations made from
-- it. Design in design/14-launch-handoff.md.
--
-- game_server is the pool. A server registers itself and then heartbeats; the Allocator
-- reserves it for a party and marks it busy while players are connected. state is the
-- server's own lifecycle, deliberately separate from an allocation's status, because a
-- reserved server can be released without ever going busy.
--
-- allocation is one party's hold on one server. player_ids is capped at 8 (the largest
-- party), and the unique partial index below is what makes the reservation request
-- idempotent: a retried POST returns the party's existing live allocation instead of
-- taking a second server.
--
-- callback_pending is the outbox flag. When an allocation ends, Session has to be told;
-- the row stays flagged until Session acknowledges, so a crash between "ended" and
-- "notified" is recovered by the reaper rather than losing the event.
--
-- party_id is opaque to this schema: a party belongs to Session, not to the Allocator,
-- so there is no foreign key to it.

-- +goose Up
CREATE TABLE game_server (
    server_id         text PRIMARY KEY CHECK (server_id ~ '^[a-z0-9][a-z0-9_.-]{0,62}$'),
    internal_addr     text NOT NULL,
    capacity          smallint NOT NULL CHECK (capacity BETWEEN 1 AND 64),
    state             text NOT NULL CHECK (state IN ('free','reserved','busy','dead')),
    allocation_id     uuid,
    players_connected smallint NOT NULL DEFAULT 0 CHECK (players_connected >= 0),
    registered_at     timestamptz NOT NULL DEFAULT now(),
    last_heartbeat    timestamptz NOT NULL DEFAULT now()
);
-- The reservation query reads free servers in server_id order.
CREATE INDEX game_server_free_idx ON game_server (server_id) WHERE state = 'free';

CREATE TABLE allocation (
    allocation_id    uuid PRIMARY KEY,
    party_id         uuid NOT NULL,
    server_id        text NOT NULL REFERENCES game_server (server_id),
    player_ids       uuid[] NOT NULL CHECK (cardinality(player_ids) BETWEEN 1 AND 8),
    status           text NOT NULL CHECK (status IN ('reserved','active','ended','expired')),
    end_reason       text CHECK (end_reason IN ('ended','expired','server_dead','server_restarted')),
    created_at       timestamptz NOT NULL DEFAULT now(),
    expires_at       timestamptz NOT NULL,
    ended_at         timestamptz,
    callback_pending boolean NOT NULL DEFAULT false,
    CHECK ((status IN ('ended','expired')) = (ended_at IS NOT NULL)),
    CHECK ((status IN ('ended','expired')) = (end_reason IS NOT NULL))
);
-- One live allocation per party: what makes POST /internal/allocations idempotent.
CREATE UNIQUE INDEX allocation_live_party ON allocation (party_id) WHERE status IN ('reserved','active');
-- GET /internal/allocations?party_id= returns the latest.
CREATE INDEX allocation_party_created_idx ON allocation (party_id, created_at DESC);
-- The reaper's expiry scan.
CREATE INDEX allocation_reserved_expiry_idx ON allocation (expires_at) WHERE status = 'reserved';
-- Outbox: ended allocations whose Session callback has not been acknowledged yet.
CREATE INDEX allocation_callback_pending_idx ON allocation (ended_at) WHERE callback_pending;

-- +goose Down
DROP TABLE allocation;
DROP TABLE game_server;
