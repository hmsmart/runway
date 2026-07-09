-- +goose Up
-- Track whether a transaction has been announced in Telegram. The drain
-- worker sends rows with notified = 0 and marks them as it goes, so a crash
-- mid-drain resumes where it left off. Existing rows were handled (or
-- deliberately suppressed) under the old callback scheme — don't replay them.
ALTER TABLE transactions ADD COLUMN notified INTEGER NOT NULL DEFAULT 0;
UPDATE transactions SET notified = 1;

-- +goose Down
ALTER TABLE transactions DROP COLUMN notified;
