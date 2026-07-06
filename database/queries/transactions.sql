-- name: UpsertTransaction :exec
INSERT INTO transactions (transaction_id, account_id, date, amount, name, merchant_name, category_primary, category_detailed, payment_channel, pending, raw_json)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(transaction_id) DO UPDATE SET
  account_id = excluded.account_id,
  date = excluded.date,
  amount = excluded.amount,
  name = excluded.name,
  merchant_name = excluded.merchant_name,
  category_primary = excluded.category_primary,
  category_detailed = excluded.category_detailed,
  payment_channel = excluded.payment_channel,
  pending = excluded.pending,
  raw_json = excluded.raw_json;
-- name: DeleteTransaction :exec
DELETE FROM transactions WHERE transaction_id = ?;
