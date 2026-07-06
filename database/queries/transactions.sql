-- name: UpsertTransaction :exec
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
    raw_json = excluded.raw_json;

-- name: SoftDeleteTransaction :exec
UPDATE transactions SET removed_at = datetime('now')
WHERE plaid_tx_id = ?;

-- name: PromotePending :exec
UPDATE transactions
SET plaid_tx_id = ?,
    date = ?,
    amount = ?,
    name = ?,
    merchant_name = ?,
    pending = 0,
    raw_json = ?
WHERE plaid_tx_id = ? AND pending = 1;