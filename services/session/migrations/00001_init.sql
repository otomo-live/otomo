-- +goose Up
-- The M1 session schema: design/04-session-minimal.md §3.1 verbatim, plus the audit_log
-- table SES-A5 asks for, which is the same shape as Config's audit_log — one table per
-- service, so a Dashboard reading both unions them without translating columns.
--
-- Four indexes beyond what §3.1 states, recorded here rather than added silently:
--
--   * friendship_hi_idx and party_member_party_idx exist because a composite primary
--     key serves only a prefix of itself. "Everything player X is in" needs the pair
--     searched from either side, and "the members of party P" needs the reverse of
--     party_member's key; without these, both read the whole table.
--   * party_invite_expires_idx is what SES-D5's cleanup job scans on, every minute.
--   * player_name_uq is §3.1's own, listed here for completeness: expression uniqueness
--     cannot be a table constraint.
--
-- No index on block: its primary key is already the only lookups there are, "did A block
-- B" and "whom has A blocked", and the latter is a prefix of the key.
--
-- Constraints beyond §3.1 are limited to facts the database can state exactly and the
-- application cannot get wrong: a party cannot be its own member's opposite, a block is
-- not self-directed, a discriminator is in range, a name is not empty. The 3–16
-- character name rule is deliberately *not* a CHECK, because §3.1 measures it in
-- characters and SES-B2 owns the Unicode policy — encoding that policy a second time in
-- SQL would mean two places to change and a migration to change one of them.

-- A player's public identity, created on first login with a provisional name (SES-B1).
-- player_id is Auth's `sub`, which is why there is no local sequence or surrogate key:
-- Auth owns the player id and this service never mints one.
CREATE TABLE player_profile (
    player_id     uuid PRIMARY KEY,
    display_name  text NOT NULL CHECK (display_name <> ''),
    discriminator smallint NOT NULL CHECK (discriminator BETWEEN 1 AND 9999),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- Duplicate display names are allowed; the discriminator disambiguates them. Uniqueness
-- is on the case-folded name, so "Tanuki#0001" and "tanuki#0001" cannot both exist --
-- otherwise a player could not be added by the name they see someone else holding.
CREATE UNIQUE INDEX player_name_uq ON player_profile (lower(display_name), discriminator);

-- One row per pair. player_lo < player_hi is the constraint that makes the pair
-- order-independent: A→B and B→A resolve to the same row, so a mutual request cannot
-- become two friendships (SES-C1), and the primary key alone prevents duplicates.
CREATE TABLE friendship (
    player_lo    uuid NOT NULL REFERENCES player_profile (player_id),
    player_hi    uuid NOT NULL REFERENCES player_profile (player_id),
    state        text NOT NULL CHECK (state IN ('pending', 'accepted')),
    requested_by uuid NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (player_lo, player_hi),
    CHECK (player_lo < player_hi)
);

-- The primary key orders the pair, so it serves "friendships of player_lo" only. Every
-- listing also has to search from the other side, and friendships are symmetric.
CREATE INDEX friendship_hi_idx ON friendship (player_hi);

CREATE TABLE block (
    blocker    uuid NOT NULL,
    blocked    uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (blocker, blocked),
    CHECK (blocker <> blocked)
);

-- A party holds no game settings: §1 puts lobbies with settings in the Matchmaker
-- milestone. What it holds for now is membership; `leader_id` is duplicated here rather
-- than derived from party_member so that a leader's departure promotes someone in the
-- same transaction that removes them (SES-D4), and so that "who leads" is answerable
-- without reading the member list.
CREATE TABLE party (
    party_id   uuid PRIMARY KEY,
    leader_id  uuid NOT NULL,
    max_size   smallint NOT NULL DEFAULT 4,
    revision   int NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- The primary key *is* the rule "one party per player" (SES-D1): joining a second party
-- is a unique violation rather than something a handler has to remember to check. The
-- foreign key cascades, so a disbanded party takes its membership with it.
CREATE TABLE party_member (
    player_id uuid PRIMARY KEY,
    party_id  uuid NOT NULL REFERENCES party (party_id) ON DELETE CASCADE,
    joined_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX party_member_party_idx ON party_member (party_id);

-- "Longest-standing member" is what promotion means (SES-D4), which makes joined_at an
-- ordering column and this the index that reads it. It is not unique: two players can
-- join in the same microsecond, and the tie is broken by player_id in the query rather
-- than by refusing the insert.
CREATE INDEX party_member_joined_idx ON party_member (party_id, joined_at, player_id);

-- One live invite per (party, invitee): re-inviting refreshes expires_at instead of
-- creating a second row (SES-D2), which the unique key enforces.
CREATE TABLE party_invite (
    invite_id   uuid PRIMARY KEY,
    party_id    uuid NOT NULL REFERENCES party (party_id) ON DELETE CASCADE,
    from_player uuid NOT NULL,
    to_player   uuid NOT NULL,
    expires_at  timestamptz NOT NULL,
    UNIQUE (party_id, to_player)
);

-- SES-D5's cleanup job deletes by expiry, every minute.
CREATE INDEX party_invite_expires_idx ON party_invite (expires_at);

-- SES-A5: staff actions, written in the same transaction as the change they describe.
-- Text columns rather than the uuids used elsewhere, because actor_id is a *staff*
-- account id from PHP Admin Auth, which this service does not own and must not assume is
-- a uuid — and because the log is read long after the rows it names may be gone, so
-- nothing here is a foreign key.
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
DROP TABLE party_invite;
DROP TABLE party_member;
DROP TABLE party;
DROP TABLE block;
DROP TABLE friendship;
DROP TABLE player_profile;
