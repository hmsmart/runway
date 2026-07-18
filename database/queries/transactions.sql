-- name: UpsertTransaction :one
INSERT INTO transactions (
    tx_id, plaid_tx_id, account_id, date, authorized_date, amount,
    name, merchant_name, category_primary, category_detailed, category_confidence,
    payment_channel, pending, raw_json, notified, merge_candidate_tx_id
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(plaid_tx_id) DO UPDATE SET
    date = excluded.date,
    -- Plaid's authorized date wins when present, but a null must not clobber
    -- a date recovered via adoption: cursor replays resend all history with
    -- authorized_date null for institutions that never supply it.
    authorized_date = COALESCE(excluded.authorized_date, authorized_date),
    amount = excluded.amount,
    name = excluded.name,
    merchant_name = excluded.merchant_name,
    category_primary = excluded.category_primary,
    category_detailed = excluded.category_detailed,
    category_confidence = excluded.category_confidence,
    payment_channel = excluded.payment_channel,
    pending = excluded.pending,
    removed_at = NULL,
    raw_json = excluded.raw_json,
    -- A replay must not wipe an offer the user hasn't acted on.
    merge_candidate_tx_id = COALESCE(excluded.merge_candidate_tx_id, merge_candidate_tx_id)
RETURNING tx_id;

-- name: AdoptPendingTransaction :execresult
-- When a pending transaction settles, Plaid removes the pending row and sends
-- the posted version under a new id, with pending_transaction_id pointing at
-- the old one. Adopting rewrites the pending row in place instead of
-- insert-plus-soft-delete: the internal tx_id survives, so chat message
-- buttons, exclusions, spreads, and the notified flag all carry forward, and
-- the pending row's date (the day the card was used) is preserved as the
-- authorized date unless Plaid supplies a better one. The NOT EXISTS guard
-- skips adoption when the posted id already has its own row; the caller
-- falls back to a plain upsert. (ASCII-only comment: sqlc miscounts
-- multibyte chars and truncates the generated query.)
UPDATE transactions SET
    plaid_tx_id = sqlc.arg(posted_plaid_id),
    date = sqlc.arg(date),
    authorized_date = COALESCE(sqlc.arg(authorized_date), authorized_date, date),
    amount = sqlc.arg(amount),
    name = sqlc.arg(name),
    merchant_name = sqlc.arg(merchant_name),
    category_primary = sqlc.arg(category_primary),
    category_detailed = sqlc.arg(category_detailed),
    category_confidence = sqlc.arg(category_confidence),
    payment_channel = sqlc.arg(payment_channel),
    pending = 0,
    removed_at = NULL,
    message_stale = 1,
    raw_json = sqlc.arg(raw_json)
WHERE transactions.plaid_tx_id = sqlc.arg(pending_plaid_id)
  AND NOT EXISTS (SELECT 1 FROM transactions t2 WHERE t2.plaid_tx_id = sqlc.arg(posted_plaid_id));

-- name: AdoptSettledTransactionByMatch :execresult
-- Fallback adoption for institutions that reissue pending transaction ids.
-- A reissued pending can be created and settled between two of our syncs, so
-- its add and remove collapse out of the stream: the settled transaction then
-- names a pending id we never received, or carries none at all, and
-- AdoptPendingTransaction has nothing to match. Instead of inserting a
-- duplicate (which re-announces and strands the old row's spread/exclusions),
-- claim the nearest unclaimed pending row on the same account with the same
-- amount whose swipe date sits within a settlement window of the posted
-- transaction's effective date. Nearest-date-first ordering keeps same-amount
-- purchases on different days paired with the right settlement. Same SET list
-- and posted-id guard as AdoptPendingTransaction. (ASCII-only comment: sqlc
-- miscounts multibyte chars and truncates the generated query.)
UPDATE transactions SET
    plaid_tx_id = sqlc.arg(posted_plaid_id),
    date = sqlc.arg(date),
    authorized_date = COALESCE(sqlc.arg(authorized_date), authorized_date, date),
    amount = sqlc.arg(amount),
    name = sqlc.arg(name),
    merchant_name = sqlc.arg(merchant_name),
    category_primary = sqlc.arg(category_primary),
    category_detailed = sqlc.arg(category_detailed),
    category_confidence = sqlc.arg(category_confidence),
    payment_channel = sqlc.arg(payment_channel),
    pending = 0,
    removed_at = NULL,
    message_stale = 1,
    raw_json = sqlc.arg(raw_json)
WHERE transactions.tx_id = (
    SELECT p.tx_id FROM transactions p
    WHERE p.pending = 1
      AND p.account_id = sqlc.arg(account_id)
      AND p.amount = sqlc.arg(amount)
      AND COALESCE(p.authorized_date, p.date)
          BETWEEN date(CAST(sqlc.arg(effective_date) AS TEXT), '-7 days')
              AND CAST(sqlc.arg(effective_date) AS TEXT)
    ORDER BY COALESCE(p.authorized_date, p.date) DESC, p.tx_id ASC
    LIMIT 1
)
  AND NOT EXISTS (SELECT 1 FROM transactions t2 WHERE t2.plaid_tx_id = sqlc.arg(posted_plaid_id));

-- name: BackfillAuthorizedDate :exec
-- Recovery path for cursor replays: a resent settled transaction whose
-- posted row already exists skips adoption, but its pending sibling may
-- still be in the table (soft-deleted by the original settle). Lift that
-- sibling's date into the posted row's empty authorized date. A missing
-- sibling writes NULL over NULL, which is a no-op.
UPDATE transactions
SET authorized_date = (
    SELECT COALESCE(p.authorized_date, p.date)
    FROM transactions p WHERE p.plaid_tx_id = sqlc.arg(pending_plaid_id)
)
WHERE transactions.plaid_tx_id = sqlc.arg(posted_plaid_id)
  AND transactions.authorized_date IS NULL;

-- name: GetTransaction :one
SELECT * FROM transactions WHERE tx_id = ?;

-- name: SoftDeleteTransaction :exec
-- Flags the card too: a removed row's announcement is now wrong (most often a
-- pending that fell off - a preauth hold - without settling into its card),
-- so the drain worker strikes the card through.
UPDATE transactions SET removed_at = datetime('now'), message_stale = 1
WHERE plaid_tx_id = ?;

-- name: FindTipRangeCandidate :one
-- An unclaimed pending row that looks like the tip-adjusted original of a
-- settled transaction: same account, swipe date within the settlement window,
-- and the settled amount above the pending amount but within 1.5x of it -
-- restaurant tips land here, while a gas-station $1 preauth against a $45
-- settlement stays out and is left to fall off. Exact amounts never reach
-- this query; they auto-adopt. The match is only recorded as an offer on the
-- settled row; the user confirms from the chat card.
SELECT p.tx_id FROM transactions p
WHERE p.pending = 1
  AND p.account_id = sqlc.arg(account_id)
  AND CAST(sqlc.arg(amount) AS REAL) > p.amount
  AND CAST(sqlc.arg(amount) AS REAL) <= p.amount * 1.5
  AND COALESCE(p.authorized_date, p.date)
      BETWEEN date(CAST(sqlc.arg(effective_date) AS TEXT), '-7 days')
          AND CAST(sqlc.arg(effective_date) AS TEXT)
ORDER BY COALESCE(p.authorized_date, p.date) DESC, p.tx_id ASC
LIMIT 1;

-- name: DeleteTransactionByTxID :exec
DELETE FROM transactions WHERE tx_id = ?;

-- name: ClearMergeCandidate :exec
UPDATE transactions SET merge_candidate_tx_id = NULL WHERE tx_id = ?;

-- name: MergeSettledIntoPending :execresult
-- User-confirmed merge: fold a settled row's identity and values onto its
-- pending sibling, keeping the pending row's tx_id (spread, exclusion, card).
-- The caller deletes the settled row in the same database transaction first,
-- so the unique plaid id is free to move. The amount is the settled one -
-- that is the whole point of a tip merge. Guards mirror the adopt queries:
-- the pending row must still be unclaimed and the posted id unused.
UPDATE transactions SET
    plaid_tx_id = sqlc.arg(posted_plaid_id),
    date = sqlc.arg(date),
    authorized_date = COALESCE(sqlc.arg(authorized_date), authorized_date, date),
    amount = sqlc.arg(amount),
    name = sqlc.arg(name),
    merchant_name = sqlc.arg(merchant_name),
    category_primary = sqlc.arg(category_primary),
    category_detailed = sqlc.arg(category_detailed),
    category_confidence = sqlc.arg(category_confidence),
    payment_channel = sqlc.arg(payment_channel),
    pending = 0,
    removed_at = NULL,
    message_stale = 1,
    raw_json = sqlc.arg(raw_json)
WHERE transactions.tx_id = sqlc.arg(pending_tx_id)
  AND transactions.pending = 1
  AND NOT EXISTS (SELECT 1 FROM transactions t2 WHERE t2.plaid_tx_id = sqlc.arg(posted_plaid_id));

-- name: SetExcluded :exec
UPDATE transactions SET excluded = ? WHERE tx_id = ?;

-- name: MarkUnnotifiableTransactions :exec
-- Retire pending rows that will never be announced: credits, soft-deleted
-- rows, and anything older than the notify window (cutoff is YYYY-MM-DD).
UPDATE transactions SET notified = 1
WHERE notified = 0
  AND (amount < 0 OR removed_at IS NOT NULL OR date < CAST(sqlc.arg(cutoff) AS TEXT));

-- name: GetPendingNotifications :many
SELECT sqlc.embed(transactions) FROM transactions
JOIN accounts ON accounts.account_id = transactions.account_id
JOIN items ON items.item_id = accounts.item_id
JOIN users ON users.id = items.user_id
WHERE users.tg_id = ? AND transactions.notified = 0 AND transactions.removed_at IS NULL
ORDER BY transactions.date ASC, transactions.tx_id ASC
LIMIT 50;

-- name: CountPendingNotifications :one
SELECT COUNT(*) FROM transactions
JOIN accounts ON accounts.account_id = transactions.account_id
JOIN items ON items.item_id = accounts.item_id
JOIN users ON users.id = items.user_id
WHERE users.tg_id = ? AND transactions.notified = 0 AND transactions.removed_at IS NULL
  AND transactions.amount >= 0 AND transactions.date >= CAST(sqlc.arg(cutoff) AS TEXT);

-- name: ListChatsWithPendingNotifications :many
SELECT DISTINCT users.tg_id FROM transactions
JOIN accounts ON accounts.account_id = transactions.account_id
JOIN items ON items.item_id = accounts.item_id
JOIN users ON users.id = items.user_id
WHERE users.tg_id IS NOT NULL AND transactions.notified = 0 AND transactions.removed_at IS NULL;

-- name: MarkTransactionNotified :exec
-- Records the announcement outcome. tg_message_id is NULL when the send
-- failed permanently; COALESCE keeps any previously recorded card in that
-- case. message_stale is deliberately left alone: if an adoption landed
-- between rendering the card and this write, the stale sweep still owes the
-- card an edit.
UPDATE transactions SET notified = 1,
    tg_message_id = COALESCE(sqlc.arg(tg_message_id), tg_message_id)
WHERE tx_id = sqlc.arg(tx_id);

-- name: ListStaleMessages :many
-- Rows whose announcement card no longer matches the database: a pending
-- settled via adoption, or a removed row (fell-off preauth) whose card gets
-- struck through. Removed rows are deliberately included, unlike the notify
-- queries. Rows with no recorded card just get the flag cleared.
SELECT sqlc.embed(transactions) FROM transactions
JOIN accounts ON accounts.account_id = transactions.account_id
JOIN items ON items.item_id = accounts.item_id
JOIN users ON users.id = items.user_id
WHERE users.tg_id = ? AND transactions.message_stale = 1
ORDER BY transactions.date ASC, transactions.tx_id ASC
LIMIT 50;

-- name: ClearMessageStale :exec
UPDATE transactions SET message_stale = 0 WHERE tx_id = ?;

-- name: DeleteTransactionsByItem :exec
DELETE FROM transactions WHERE account_id IN (SELECT account_id FROM accounts WHERE item_id = ?);

-- name: SetAmortEnd :exec
UPDATE transactions SET amort_end = date("date", CAST(sqlc.arg(modifier) AS TEXT)) WHERE tx_id = sqlc.arg(tx_id);

-- name: ClearAmortEnd :exec
UPDATE transactions SET amort_end = NULL WHERE tx_id = ?;

-- name: ListTransactionsByUser :many
-- Feeds the /transactions table. Unlike ListSpendTransactionsByUser this
-- includes credits and excluded rows: the table renders (and strikes
-- through) exclusions rather than hiding them.
SELECT
    t.tx_id,
    CAST(COALESCE(t.authorized_date, t.date) AS TEXT) AS date,
    a.name AS account_name,
    COALESCE(t.merchant_name, t.name) AS description,
    t.amount,
    t.excluded,
    t.date AS raw_date,
    t.amort_end
FROM transactions t
JOIN accounts a ON a.account_id = t.account_id
JOIN items i ON i.item_id = a.item_id
WHERE i.user_id = ?
  AND t.removed_at IS NULL
  AND a.tracked = 1
ORDER BY date DESC, t.tx_id DESC;