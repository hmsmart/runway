-- +goose Up
-- tg_message_id records which chat message announced a transaction, so the
-- app can go back and edit the card when the row changes underneath it.
-- message_stale flags that the card no longer matches the database (set by
-- the adopt queries when a pending settles); the drain worker edits the card
-- and clears the flag.
ALTER TABLE transactions ADD COLUMN tg_message_id INTEGER;
ALTER TABLE transactions ADD COLUMN message_stale INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE transactions DROP COLUMN message_stale;
ALTER TABLE transactions DROP COLUMN tg_message_id;
