-- name: ListActiveBillsByUser :many
SELECT * FROM bills
WHERE user_id = @user_id AND active = 1
ORDER BY day_of_month;

-- name: InsertBill :exec
INSERT INTO bills (id, user_id, name, amount, day_of_month)
VALUES (@id, @user_id, @name, @amount, @day_of_month);

-- name: DeactivateBillByName :execresult
UPDATE bills SET active = 0
WHERE user_id = @user_id AND UPPER(name) = UPPER(@name) AND active = 1;

-- name: DeactivateBillByID :exec
UPDATE bills SET active = 0
WHERE id = @id AND user_id = @user_id;
