-- name: UpsertTransaction :one
INSERT INTO transactions (
    tx_id, plaid_tx_id, account_id, date, amount,
    name, merchant_name, category_primary, category_detailed,
    payment_channel, pending, raw_json
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(plaid_tx_id) DO UPDATE SET
    date = excluded.date,
    amount = excluded.amount,
    name = excluded.name,
    merchant_name = excluded.merchant_name,
    category_primary = excluded.category_primary,
    category_detailed = excluded.category_detailed,
    payment_channel = excluded.payment_channel,
    pending = excluded.pending,
    removed_at = NULL,
    raw_json = excluded.raw_json
RETURNING tx_id;

-- name: GetTransaction :one
SELECT * FROM transactions WHERE tx_id = ?;

-- name: SoftDeleteTransaction :exec
UPDATE transactions SET removed_at = datetime('now')
WHERE plaid_tx_id = ?;

-- name: SetExcluded :exec
UPDATE transactions SET excluded = ? WHERE tx_id = ?;

-- name: SetAmortEnd :exec
UPDATE transactions SET amort_end = date("date", CAST(sqlc.arg(modifier) AS TEXT)) WHERE tx_id = sqlc.arg(tx_id);
