-- +goose Up
-- tg_message_id records which chat message announced a transaction, so the
-- app can go back and edit the card when the row changes underneath it.
-- message_stale flags that the card no longer matches the database (set by
-- the adopt queries when a pending settles); the drain worker edits the card
-- and clears the flag.
-- merge_candidate_tx_id links a settled row to an unclaimed pending row that
-- looks like its tip-adjusted settlement (same account, close date, amount
-- within tip range). The chat card offers a one-tap merge; nothing merges
-- without the user confirming.
-- logo_url is Plaid's merchant logo (falling back to its generic category
-- icon), shown in the dashboard table. Plaintext is fine: it encodes nothing
-- the plaintext merchant_name column doesn't already say.
ALTER TABLE transactions ADD COLUMN tg_message_id INTEGER;
ALTER TABLE transactions ADD COLUMN message_stale INTEGER NOT NULL DEFAULT 0;
ALTER TABLE transactions ADD COLUMN merge_candidate_tx_id TEXT;
ALTER TABLE transactions ADD COLUMN logo_url TEXT;

-- +goose Down
ALTER TABLE transactions DROP COLUMN logo_url;
ALTER TABLE transactions DROP COLUMN merge_candidate_tx_id;
ALTER TABLE transactions DROP COLUMN message_stale;
ALTER TABLE transactions DROP COLUMN tg_message_id;
