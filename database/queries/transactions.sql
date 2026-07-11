-- name: UpsertTransaction :one
INSERT INTO transactions (
    tx_id, plaid_tx_id, account_id, date, authorized_date, amount,
    name, merchant_name, category_primary, category_detailed, category_confidence,
    payment_channel, pending, raw_json, notified
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(plaid_tx_id) DO UPDATE SET
    date = excluded.date,
    -- Plaid's authorized date wins when present, but a null must not clobber
    -- a date recovered via adoption: cursor replays resend all history with
    -- authorized_date null for institutions that never supply it.
    authorized_date = COALESCE(excluded.authorized_date, authorized_date),
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

-- name: AdoptPendingTransaction :execresult
-- When a pending transaction settles, Plaid removes the pending row and sends
-- the posted version under a new id, with pending_transaction_id pointing at
-- the old one. Adopting rewrites the pending row in place instead of
-- insert-plus-soft-delete: the internal tx_id survives, so chat message
-- buttons, exclusions, spreads, and the notified flag all carry forward, and
-- the pending row's date (the day the card was used) is preserved as the
-- authorized date unless Plaid supplies a better one. The NOT EXISTS guard
-- skips adoption when the posted id already has its own row; the caller
-- falls back to a plain upsert. (ASCII-only comment: sqlc miscounts
-- multibyte chars and truncates the generated query.)
UPDATE transactions SET
    plaid_tx_id = sqlc.arg(posted_plaid_id),
    date = sqlc.arg(date),
    authorized_date = COALESCE(sqlc.arg(authorized_date), authorized_date, date),
    amount = sqlc.arg(amount),
    name = sqlc.arg(name),
    merchant_name = sqlc.arg(merchant_name),
    category_primary = sqlc.arg(category_primary),
    category_detailed = sqlc.arg(category_detailed),
    category_confidence = sqlc.arg(category_confidence),
    payment_channel = sqlc.arg(payment_channel),
    pending = 0,
    removed_at = NULL,
    raw_json = sqlc.arg(raw_json)
WHERE transactions.plaid_tx_id = sqlc.arg(pending_plaid_id)
  AND NOT EXISTS (SELECT 1 FROM transactions t2 WHERE t2.plaid_tx_id = sqlc.arg(posted_plaid_id));

-- name: BackfillAuthorizedDate :exec
-- Recovery path for cursor replays: a resent settled transaction whose
-- posted row already exists skips adoption, but its pending sibling may
-- still be in the table (soft-deleted by the original settle). Lift that
-- sibling's date into the posted row's empty authorized date. A missing
-- sibling writes NULL over NULL, which is a no-op.
UPDATE transactions
SET authorized_date = (
    SELECT COALESCE(p.authorized_date, p.date)
    FROM transactions p WHERE p.plaid_tx_id = sqlc.arg(pending_plaid_id)
)
WHERE transactions.plaid_tx_id = sqlc.arg(posted_plaid_id)
  AND transactions.authorized_date IS NULL;

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