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

-- name: SetReportSchedule :exec
UPDATE users SET report_time = ?, report_sent_on = ? WHERE id = ?;

-- name: MarkReportSent :exec
UPDATE users SET report_sent_on = ? WHERE id = ?;

-- name: ListDueReports :many
-- Users whose scheduled report time has passed today and who haven't been
-- sent one yet today. Both comparisons are string comparisons over
-- zero-padded "HH:MM" and "YYYY-MM-DD" values.
SELECT id, tg_id FROM users
WHERE active = 1
  AND tg_id IS NOT NULL
  AND report_time IS NOT NULL
  AND report_time <= ?
  AND (report_sent_on IS NULL OR report_sent_on < ?);

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

-- name: GetUserByAPIKey :one
SELECT * FROM users WHERE api_key = ? AND active = 1;

-- name: SetUserAPIKey :exec
UPDATE users SET api_key = ? WHERE id = ?;