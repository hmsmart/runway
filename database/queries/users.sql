-- name: GetUserByTelegram :one
SELECT * FROM users WHERE tg_id = ? and active = 1
