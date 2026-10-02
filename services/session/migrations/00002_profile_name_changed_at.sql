-- +goose Up
-- SE-2: the rename limit. PATCH /me refuses a second rename within
-- session.rules names.rename_cooldown_hours (24 h by default) of the last one, so the
-- time of the last rename has to be stored.
--
-- NULL means the player has never chosen a name: the provisional name POST /me/init
-- assigns does not start the cooldown, so a new player can pick their name at once.
ALTER TABLE player_profile ADD COLUMN name_changed_at timestamptz;

-- +goose Down
ALTER TABLE player_profile DROP COLUMN name_changed_at;
