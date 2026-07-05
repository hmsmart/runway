-- name: GetItemByID :one
SELECT * FROM items WHERE item_id = ?;
-- name: GetAllItems :many
SELECT * FROM items WHERE status = 'active' ORDER BY created_at DESC;
-- name: CreateItem :exec
INSERT INTO items (
    item_id,
    access_token,
    institution_name,
    status
) VALUES (
    ?,
    ?,
    ?,
    ?
);