-- +goose Up
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS "items" (
	"item_id"	TEXT NOT NULL UNIQUE,
	"access_token"	TEXT NOT NULL,
	"institution_name"	TEXT,
	"status"	TEXT NOT NULL DEFAULT 'active',
	"cursor"	TEXT,
	"created_at"	TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	"last_synced_at"	TIMESTAMP,
	PRIMARY KEY("item_id")
);
CREATE TABLE IF NOT EXISTS "accounts" (
	"account_id"	TEXT NOT NULL,
	"item_id"	TEXT NOT NULL,
	"name"	TEXT NOT NULL,
	"mask"	TEXT,
	"balance_available"	REAL,
	"balance_current"	REAL,
	"iso_currency_code"	TEXT,
	"type"	TEXT,
	"subtype"	TEXT,
	"raw_json"	TEXT,
	"created_at"	TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	"last_synced_at"	TIMESTAMP,
	PRIMARY KEY("account_id"),
	FOREIGN KEY("item_id") REFERENCES "items"("item_id")
);
CREATE TABLE IF NOT EXISTS "daily_spend" (
    "date"              TEXT NOT NULL,
    "account_id"        TEXT NOT NULL,
    "spend"             REAL NOT NULL DEFAULT 0.0,
    "settled_spend"     REAL NOT NULL DEFAULT 0.0,
    "ema_14"            REAL,
    "ema_28"            REAL,
    PRIMARY KEY("date", "account_id"),
    FOREIGN KEY("account_id") REFERENCES "accounts"("account_id")
);
CREATE TABLE IF NOT EXISTS "transactions" (
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

-- +goose Down
DROP TABLE "transactions";
DROP TABLE "daily_spend";
DROP TABLE "accounts";
DROP TABLE "items";