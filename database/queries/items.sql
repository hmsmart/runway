-- name: GetItemByID :one
SELECT * FROM items WHERE item_id = ?;
-- name: GetAllItems :many
SELECT * FROM items WHERE status = 'active' ORDER BY created_at DESC;
-- name: CreateItem :exec
INSERT INTO items (
    item_id,
    user_id,
    access_token,
    institution_name,
    status
) VALUES (
    ?,
    ?,
    ?,
    ?,
    ?
);
-- name: CountItemsByUser :one
SELECT COUNT(*) FROM items WHERE user_id = ? AND status = 'active';
-- name: ListItemsByUser :many
-- Stable order so /links indices stay consistent between list and unlink.
SELECT * FROM items WHERE user_id = ? AND status = 'active' ORDER BY created_at ASC, item_id ASC;
-- name: GetManualItemByUser :one
-- The hidden container for user-entered (/receipt) transactions. status is
-- 'manual', which keeps it out of every status='active' query: the sync
-- loop, /accounts, /unlink, and the item count.
SELECT * FROM items WHERE user_id = ? AND status = 'manual';
-- name: DeleteItem :exec
DELETE FROM items WHERE item_id = ?;
-- name: UpdateItemCursor :exec
UPDATE items SET cursor = ?, last_synced_at = ? WHERE item_id = ?;
