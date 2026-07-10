-- +goose Up
-- Rework daily_spend from per-account to per-user: spend is tracked against a
-- single monthly budget across all of a user's accounts. Rows are derived
-- entirely from the transactions table (the series is recomputed in full on
-- every change), so drop-and-recreate loses nothing. The cascade keeps
-- /unregister's DeleteUser working without knowing about this table.
DROP TABLE daily_spend;
CREATE TABLE daily_spend (
    "date"     TEXT NOT NULL,
    "user_id"  TEXT NOT NULL,
    "spend"    REAL NOT NULL DEFAULT 0.0,
    "ema_14"   REAL,
    "ema_28"   REAL,
    PRIMARY KEY ("date", "user_id"),
    FOREIGN KEY ("user_id") REFERENCES "users"("id") ON DELETE CASCADE
);

-- +goose Down
DROP TABLE daily_spend;
CREATE TABLE daily_spend (
    "date"              TEXT NOT NULL,
    "account_id"        TEXT NOT NULL,
    "spend"             REAL NOT NULL DEFAULT 0.0,
    "settled_spend"     REAL NOT NULL DEFAULT 0.0,
    "ema_14"            REAL,
    "ema_28"            REAL,
    PRIMARY KEY("date", "account_id"),
    FOREIGN KEY("account_id") REFERENCES "accounts"("account_id")
);
