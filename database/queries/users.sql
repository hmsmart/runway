-- name: GetUserByTelegram :one
SELECT * FROM users WHERE tg_id = ? and active = 1;

-- name: RedeemInviteCode :execresult
UPDATE users
SET tg_id = ?, active = 1
WHERE invite_code = ? AND tg_id IS NULL;