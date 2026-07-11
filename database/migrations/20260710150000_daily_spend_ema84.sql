-- ema_84 joins the series as the long-horizon baseline the 28-day rate is
-- judged against in /runway. Rows are derived data rebuilt in full on every
-- recompute, so existing rows just carry NULL until the next sweep fills it.
-- +goose Up
ALTER TABLE daily_spend ADD COLUMN ema_84 REAL;

-- +goose Down
ALTER TABLE daily_spend DROP COLUMN ema_84;
