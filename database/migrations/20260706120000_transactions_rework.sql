-- +goose Up
-- Rebuild transactions with an internal UUID primary key; Plaid's ID becomes a
-- unique lookup key. Rows are re-derivable from Plaid, so drop the table and
-- reset item cursors to trigger a full re-sync instead of backfilling in place.
DROP TABLE "transactions";
CREATE TABLE "transactions" (
    "tx_id"              TEXT NOT NULL,
    "plaid_tx_id"        TEXT NOT NULL UNIQUE,
    "account_id"         TEXT NOT NULL,
    "date"               TEXT NOT NULL,
    "amount"             REAL NOT NULL,
    "name"               TEXT NOT NULL,
    "merchant_name"      TEXT,
    "category_primary"   TEXT NOT NULL,
    "category_detailed"  TEXT NOT NULL,
    "category_confidence" TEXT,
    "payment_channel"    TEXT NOT NULL,
    "pending"            INTEGER NOT NULL DEFAULT 0,
    "removed_at"         TEXT,
    "amort_end"          TEXT,
    "excluded"           INTEGER NOT NULL DEFAULT 0,
    "raw_json"           TEXT,
    PRIMARY KEY("tx_id"),
    FOREIGN KEY("account_id") REFERENCES "accounts"("account_id")
);

UPDATE items SET cursor = NULL, last_synced_at = NULL;

-- +goose Down
DROP TABLE "transactions";
CREATE TABLE "transactions" (
    "transaction_id"     TEXT NOT NULL,
    "account_id"         TEXT NOT NULL,
    "date"               TEXT NOT NULL,
    "amount"             REAL NOT NULL,
    "name"               TEXT NOT NULL,
    "merchant_name"      TEXT,
    "category_primary"   TEXT,
    "category_detailed"  TEXT,
    "payment_channel"    TEXT,
    "pending"            INTEGER NOT NULL DEFAULT 0,
    "raw_json"           TEXT,
    PRIMARY KEY("transaction_id"),
    FOREIGN KEY("account_id") REFERENCES "accounts"("account_id")
);

UPDATE items SET cursor = NULL, last_synced_at = NULL;
