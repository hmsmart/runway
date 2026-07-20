-- +goose Up
CREATE TABLE IF NOT EXISTS bills (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users(id),
    name         TEXT NOT NULL,
    amount       REAL NOT NULL,
    day_of_month INTEGER NOT NULL CHECK (day_of_month BETWEEN 1 AND 31),
    active       INTEGER NOT NULL DEFAULT 1,
    created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_bills_user ON bills(user_id) WHERE active = 1;

ALTER TABLE users ADD COLUMN pay_amount REAL;
ALTER TABLE users ADD COLUMN pay_day INTEGER;

-- +goose Down
ALTER TABLE users DROP COLUMN pay_day;
ALTER TABLE users DROP COLUMN pay_amount;
DROP INDEX IF EXISTS idx_bills_user;
DROP TABLE IF EXISTS bills;
