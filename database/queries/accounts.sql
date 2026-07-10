-- name: GetAccountById :one
SELECT * FROM accounts WHERE account_id = ? AND item_id = ?;
-- name: GetAllAccounts :many
SELECT * FROM accounts WHERE tracked = 1 ORDER BY created_at DESC;
-- name: ListAccountsByItem :many
SELECT * FROM accounts WHERE item_id = ? ORDER BY name ASC;
-- name: ListTrackedAccountsByUser :many
SELECT a.* FROM accounts a
JOIN items i ON i.item_id = a.item_id
WHERE i.user_id = ? AND a.tracked = 1;
-- name: DeleteAccountsByItem :exec
DELETE FROM accounts WHERE item_id = ?;
-- name: UpsertAccount :exec
INSERT INTO accounts (account_id, item_id, name, mask, type, subtype, balance_available, balance_current, iso_currency_code, tracked, last_synced_at, raw_json)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(account_id) DO UPDATE SET
  item_id = excluded.item_id,
  name = excluded.name,
  mask = excluded.mask,
  type = excluded.type,
  subtype = excluded.subtype,
  balance_available = excluded.balance_available,
  balance_current = excluded.balance_current,
  iso_currency_code = excluded.iso_currency_code,
  last_synced_at = excluded.last_synced_at,
  raw_json = excluded.raw_json