-- Per-user daily report schedule. report_time is a zero-padded 24h "HH:MM"
-- in the server's timezone (NULL = no scheduled report); string comparison is
-- time-of-day order. report_sent_on is the last date a scheduled report went
-- out, which is what makes delivery at-most-once per day and lets a missed
-- slot (downtime, restart) send late rather than never. Column names are
-- unquoted because sqlc silently ignores quoted ALTER TABLE ADD COLUMN.
-- +goose Up
ALTER TABLE users ADD COLUMN report_time TEXT;
ALTER TABLE users ADD COLUMN report_sent_on TEXT;

-- +goose Down
ALTER TABLE users DROP COLUMN report_sent_on;
ALTER TABLE users DROP COLUMN report_time;
