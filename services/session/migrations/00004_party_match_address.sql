-- +goose Up
-- LB-3: where the party's match is. design/14-launch-handoff.md §2.1 has
-- GET /party return match {allocation_id, address, port} while in_game, and
-- POST /party/launch/ticket answer with the address and port, but the Allocator returns
-- them only in the allocation response itself (§4.2). Session keeps them from there.
-- Both are NULL outside in_game, like allocation_id.
ALTER TABLE party
  ADD COLUMN match_address text,
  ADD COLUMN match_port integer CHECK (match_port BETWEEN 1 AND 65535);

-- +goose Down
ALTER TABLE party DROP COLUMN match_port, DROP COLUMN match_address;
