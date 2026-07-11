-- name: ListSpendTransactionsByUser :many
-- The raw material for the daily-spend series: every transaction that counts
-- against the user's budget. The authorized (swipe) date wins over the posted
-- date when Plaid provides it: spend belongs to the day the card was used.
-- Credits (amount < 0) stay out because deposits and refunds land in checking
-- accounts and would swamp the spend signal. Untracked accounts are excluded
-- so the tracked toggle means something once it gets a UI. (ASCII only: sqlc
-- miscounts multibyte chars in comments and truncates the generated query.)
SELECT CAST(COALESCE(t.authorized_date, t.date) AS TEXT) AS date, t.amount, t.amort_end
FROM transactions t
JOIN accounts a ON a.account_id = t.account_id
JOIN items i ON i.item_id = a.item_id
WHERE i.user_id = ?
  AND t.excluded = 0
  AND t.removed_at IS NULL
  AND t.amount > 0
  AND a.tracked = 1;

-- name: GetDailySpendDay :one
SELECT * FROM daily_spend WHERE user_id = ? AND date = ?;

-- name: DeleteDailySpendByUser :exec
DELETE FROM daily_spend WHERE user_id = ?;

-- name: InsertDailySpend :exec
INSERT INTO daily_spend (date, user_id, spend, ema_14, ema_28, ema_84)
VALUES (?, ?, ?, ?, ?, ?);
