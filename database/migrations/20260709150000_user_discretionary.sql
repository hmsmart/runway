-- +goose Up
-- Monthly discretionary budget in dollars. NULL means the user hasn't set one
-- yet; setup requires it before an account can be linked, since the whole app
-- exists to track discretionary spend (not mortgage/insurance/etc).
ALTER TABLE users ADD COLUMN discretionary_monthly REAL;

-- +goose Down
ALTER TABLE users DROP COLUMN discretionary_monthly;
