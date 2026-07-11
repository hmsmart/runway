-- +goose Up
-- Plaid's `date` is when the transaction posted; `authorized_date` is when
-- the card was actually swiped, often days earlier. The swipe date is the
-- right day to count spend against, so store it and prefer it wherever a
-- transaction date matters. Reset item cursors so the next sync re-pulls
-- history and backfills the column: existing rows update in place via the
-- upsert (which preserves notified/excluded/amort_end), so nothing re-announces.
ALTER TABLE transactions ADD COLUMN authorized_date TEXT;
UPDATE items SET cursor = NULL, last_synced_at = NULL;

-- +goose Down
ALTER TABLE transactions DROP COLUMN authorized_date;
