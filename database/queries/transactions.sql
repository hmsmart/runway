-- name: UpsertTransaction :one
INSERT INTO transactions (
    tx_id, plaid_tx_id, account_id, date, amount,
    name, merchant_name, category_primary, category_detailed, category_confidence,
    payment_channel, pending, raw_json, notified
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(plaid_tx_id) DO UPDATE SET
    date = excluded.date,
    amount = excluded.amount,
    name = excluded.name,
    merchant_name = excluded.merchant_name,
    category_primary = excluded.category_primary,
    category_detailed = excluded.category_detailed,
    category_confidence = excluded.category_confidence,
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

-- name: MarkUnnotifiableTransactions :exec
-- Retire pending rows that will never be announced: credits, soft-deleted
-- rows, and anything older than the notify window (cutoff is YYYY-MM-DD).
UPDATE transactions SET notified = 1
WHERE notified = 0
  AND (amount < 0 OR removed_at IS NOT NULL OR date < CAST(sqlc.arg(cutoff) AS TEXT));

-- name: GetPendingNotifications :many
SELECT sqlc.embed(transactions) FROM transactions
JOIN accounts ON accounts.account_id = transactions.account_id
JOIN items ON items.item_id = accounts.item_id
JOIN users ON users.id = items.user_id
WHERE users.tg_id = ? AND transactions.notified = 0 AND transactions.removed_at IS NULL
ORDER BY transactions.date ASC, transactions.tx_id ASC
LIMIT 50;

-- name: CountPendingNotifications :one
SELECT COUNT(*) FROM transactions
JOIN accounts ON accounts.account_id = transactions.account_id
JOIN items ON items.item_id = accounts.item_id
JOIN users ON users.id = items.user_id
WHERE users.tg_id = ? AND transactions.notified = 0 AND transactions.removed_at IS NULL
  AND transactions.amount >= 0 AND transactions.date >= CAST(sqlc.arg(cutoff) AS TEXT);

-- name: ListChatsWithPendingNotifications :many
SELECT DISTINCT users.tg_id FROM transactions
JOIN accounts ON accounts.account_id = transactions.account_id
JOIN items ON items.item_id = accounts.item_id
JOIN users ON users.id = items.user_id
WHERE users.tg_id IS NOT NULL AND transactions.notified = 0 AND transactions.removed_at IS NULL;

-- name: MarkTransactionNotified :exec
UPDATE transactions SET notified = 1 WHERE tx_id = ?;

-- name: DeleteTransactionsByItem :exec
DELETE FROM transactions WHERE account_id IN (SELECT account_id FROM accounts WHERE item_id = ?);

-- name: SetAmortEnd :exec
UPDATE transactions SET amort_end = date("date", CAST(sqlc.arg(modifier) AS TEXT)) WHERE tx_id = sqlc.arg(tx_id);

-- name: ClearAmortEnd :exec
UPDATE transactions SET amort_end = NULL WHERE tx_id = ?;

-- name: SetTxDate :exec
UPDATE transactions SET date = ? WHERE tx_id = sqlc.arg(tx_id);