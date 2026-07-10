-- name: GetUserByTelegram :one
SELECT * FROM users WHERE tg_id = ? and active = 1;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = ? and active = 1;

-- name: RedeemInviteCode :execresult
UPDATE users
SET tg_id = ?, active = 1
WHERE invite_code = ? AND tg_id IS NULL;

-- name: ListActiveUserIDs :many
SELECT id FROM users WHERE active = 1;

-- name: SetDiscretionary :exec
UPDATE users SET discretionary_monthly = ? WHERE id = ?;

-- name: DeleteUser :exec
DELETE FROM users WHERE id = ?;

-- name: CreateInviteCode :exec
INSERT INTO users (
    id,
    tg_id,
    invite_code,
    active,
    can_invite
) VALUES (
    ?,
    NULL,
    ?,
    0,
    0
);