-- name: GetAccountById :one
SELECT * FROM accounts WHERE account_id = ? AND item_id = ?;
-- name: GetAllAccounts :many
SELECT * FROM accounts WHERE tracked = 1 ORDER BY created_at DESC;
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
  tracked = excluded.tracked,
  last_synced_at = excluded.last_synced_at,
  raw_json = excluded.raw_json
WHERE excluded.last_synced_at > accounts.last_synced_at OR accounts.last_synced_at IS NULL