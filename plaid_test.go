package main

import (
	"context"
	"strings"
	"testing"

	"github.com/hmsmart/runway/database"
	"github.com/hmsmart/runway/database/sqlcgen"
	"github.com/plaid/plaid-go/v43/plaid"
)

// newSyncFixture builds a store with one user/item/account so transaction
// rows satisfy their foreign keys, mirroring what syncItem guarantees by
// syncing accounts before transactions.
func newSyncFixture(t *testing.T) (*database.Store, *Config) {
	t.Helper()
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.CreateInviteCode(ctx, sqlcgen.CreateInviteCodeParams{ID: "u1", InviteCode: "ABCD2234"}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	tgID := int64(999)
	if _, err := store.RedeemInviteCode(ctx, sqlcgen.RedeemInviteCodeParams{TgID: &tgID, InviteCode: "ABCD2234"}); err != nil {
		t.Fatalf("activate user: %v", err)
	}
	if err := store.CreateItem(ctx, sqlcgen.CreateItemParams{
		ItemID: "item1", UserID: "u1", AccessToken: "tok", Status: "active",
	}); err != nil {
		t.Fatalf("create item: %v", err)
	}
	err := store.ExecTx(ctx, func(q *sqlcgen.Queries) error {
		return q.UpsertAccount(ctx, sqlcgen.UpsertAccountParams{
			AccountID: "acct1", ItemID: "item1", Name: "Checking", Tracked: 1,
		})
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	return store, &Config{DBCryptKey: make([]byte, 32)}
}

// insertPending books a pending transaction row directly, as an earlier sync
// page would have, and returns its internal tx_id.
func insertPending(t *testing.T, store *database.Store, plaidID, date string, amount float64) string {
	t.Helper()
	txID, err := store.UpsertTransaction(context.Background(), sqlcgen.UpsertTransactionParams{
		TxID:           "tx-" + plaidID,
		PlaidTxID:      plaidID,
		AccountID:      "acct1",
		Date:           date,
		AuthorizedDate: &date,
		Amount:         amount,
		Name:           "Test Merchant",
		Pending:        1,
	})
	if err != nil {
		t.Fatalf("insert pending %s: %v", plaidID, err)
	}
	return txID
}

// settledTx builds a settled (non-pending) transaction as Plaid would deliver
// it. pendingID may be "" for institutions that omit the linkage entirely.
func settledTx(plaidID, pendingID, date, authorizedDate string, amount float64) plaid.Transaction {
	tx := plaid.Transaction{}
	tx.SetTransactionId(plaidID)
	tx.SetAccountId("acct1")
	tx.SetDate(date)
	if authorizedDate != "" {
		tx.SetAuthorizedDate(authorizedDate)
	}
	tx.SetName("Test Merchant")
	tx.SetPending(false)
	tx.SetAmount(amount)
	if pendingID != "" {
		tx.SetPendingTransactionId(pendingID)
	}
	return tx
}

func persistAdded(t *testing.T, store *database.Store, cfg *Config, cursor string, txs ...plaid.Transaction) {
	t.Helper()
	resp := plaid.TransactionsSyncResponse{}
	resp.SetAdded(txs)
	if err := persistTransactionsPage(context.Background(), "item1", resp, cursor, store, cfg, 0); err != nil {
		t.Fatalf("persist page: %v", err)
	}
}

// TestFallbackAdoptsReissuedPending replays the observed production failure:
// the pending row was soft-deleted by an earlier sync's removed event, and the
// settled transaction arrived later naming a reissued pending id we never
// received. The settled row must claim the old pending row - keeping its
// tx_id, spread, and notified flag - instead of inserting a duplicate.
func TestFallbackAdoptsReissuedPending(t *testing.T) {
	store, cfg := newSyncFixture(t)
	ctx := context.Background()
	pendingTxID := insertPending(t, store, "pend-old", "2026-07-16", 41.60)
	if err := store.SetAmortEnd(ctx, sqlcgen.SetAmortEndParams{Modifier: "+7 days", TxID: pendingTxID}); err != nil {
		t.Fatalf("set amort: %v", err)
	}
	if err := store.MarkTransactionNotified(ctx, sqlcgen.MarkTransactionNotifiedParams{TxID: pendingTxID}); err != nil {
		t.Fatalf("mark notified: %v", err)
	}
	if err := store.SoftDeleteTransaction(ctx, "pend-old"); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	persistAdded(t, store, cfg, "c1", settledTx("post-1", "pend-ghost", "2026-07-18", "", 41.60))

	row, err := store.GetTransaction(ctx, pendingTxID)
	if err != nil {
		t.Fatalf("load adopted row: %v", err)
	}
	if row.PlaidTxID != "post-1" {
		t.Errorf("plaid_tx_id = %q, want post-1", row.PlaidTxID)
	}
	if row.Pending != 0 || row.RemovedAt != nil {
		t.Errorf("row should be settled and live, got pending=%d removed_at=%v", row.Pending, row.RemovedAt)
	}
	if row.AmortEnd == nil || *row.AmortEnd != "2026-07-23" {
		t.Errorf("amort_end = %v, want 2026-07-23", row.AmortEnd)
	}
	if row.Notified != 1 {
		t.Errorf("notified = %d, want 1 (no re-announcement)", row.Notified)
	}
	if row.AuthorizedDate == nil || *row.AuthorizedDate != "2026-07-16" {
		t.Errorf("authorized_date = %v, want swipe date 2026-07-16", row.AuthorizedDate)
	}
	if n := countTransactions(t, store); n != 1 {
		t.Errorf("transaction count = %d, want 1 (no duplicate)", n)
	}
}

// TestFallbackAdoptsLivePendingWithoutLinkage covers the settled transaction
// arriving with no pending_transaction_id at all while its pending sibling is
// still live - the double-count case.
func TestFallbackAdoptsLivePendingWithoutLinkage(t *testing.T) {
	store, cfg := newSyncFixture(t)
	pendingTxID := insertPending(t, store, "pend-live", "2026-07-17", 4.07)

	persistAdded(t, store, cfg, "c1", settledTx("post-2", "", "2026-07-17", "", 4.07))

	row, err := store.GetTransaction(context.Background(), pendingTxID)
	if err != nil {
		t.Fatalf("load adopted row: %v", err)
	}
	if row.PlaidTxID != "post-2" || row.Pending != 0 {
		t.Errorf("adoption missed: plaid_tx_id=%q pending=%d", row.PlaidTxID, row.Pending)
	}
	if n := countTransactions(t, store); n != 1 {
		t.Errorf("transaction count = %d, want 1 (no double count)", n)
	}
}

// TestFallbackPairsByNearestDate: two unclaimed pendings with the same amount
// on different days; the settled transaction's swipe date must pick the right
// one, not just any amount match.
func TestFallbackPairsByNearestDate(t *testing.T) {
	store, cfg := newSyncFixture(t)
	ctx := context.Background()
	earlierTxID := insertPending(t, store, "pend-jul14", "2026-07-14", 4.07)
	laterTxID := insertPending(t, store, "pend-jul15", "2026-07-15", 4.07)

	persistAdded(t, store, cfg, "c1", settledTx("post-3", "pend-ghost", "2026-07-16", "2026-07-14", 4.07))

	earlier, err := store.GetTransaction(ctx, earlierTxID)
	if err != nil {
		t.Fatalf("load earlier: %v", err)
	}
	if earlier.PlaidTxID != "post-3" {
		t.Errorf("earlier pending should be adopted, plaid_tx_id=%q", earlier.PlaidTxID)
	}
	later, err := store.GetTransaction(ctx, laterTxID)
	if err != nil {
		t.Fatalf("load later: %v", err)
	}
	if later.PlaidTxID != "pend-jul15" || later.Pending != 1 {
		t.Errorf("later pending should be untouched, got plaid_tx_id=%q pending=%d", later.PlaidTxID, later.Pending)
	}
}

// TestFallbackSkipsUnrelatedSettled: a settled transaction with no plausible
// pending sibling (different amount) must insert normally.
func TestFallbackSkipsUnrelatedSettled(t *testing.T) {
	store, cfg := newSyncFixture(t)
	pendingTxID := insertPending(t, store, "pend-other", "2026-07-17", 9.99)

	persistAdded(t, store, cfg, "c1", settledTx("post-4", "", "2026-07-17", "", 4.07))

	row, err := store.GetTransaction(context.Background(), pendingTxID)
	if err != nil {
		t.Fatalf("load pending: %v", err)
	}
	if row.PlaidTxID != "pend-other" || row.Pending != 1 {
		t.Errorf("pending should be untouched, got plaid_tx_id=%q pending=%d", row.PlaidTxID, row.Pending)
	}
	if n := countTransactions(t, store); n != 2 {
		t.Errorf("transaction count = %d, want 2", n)
	}
}

// TestFallbackIgnoresReplayedSettled: a cursor replay resends a settled
// transaction whose row already exists; the posted-id guard must keep it from
// claiming some unrelated pending row.
func TestFallbackIgnoresReplayedSettled(t *testing.T) {
	store, cfg := newSyncFixture(t)
	post := settledTx("post-5", "", "2026-07-17", "", 4.07)
	persistAdded(t, store, cfg, "c1", post)
	pendingTxID := insertPending(t, store, "pend-new", "2026-07-17", 4.07)

	persistAdded(t, store, cfg, "c2", post)

	row, err := store.GetTransaction(context.Background(), pendingTxID)
	if err != nil {
		t.Fatalf("load pending: %v", err)
	}
	if row.PlaidTxID != "pend-new" || row.Pending != 1 {
		t.Errorf("pending should be untouched by replay, got plaid_tx_id=%q pending=%d", row.PlaidTxID, row.Pending)
	}
	if n := countTransactions(t, store); n != 2 {
		t.Errorf("transaction count = %d, want 2", n)
	}
}

// TestAdoptionMarksCardStale: settling a pending must flag its announcement
// card for the drain worker's edit sweep, and the stale list must surface it
// once a card is recorded. Clearing the flag empties the list again.
func TestAdoptionMarksCardStale(t *testing.T) {
	store, cfg := newSyncFixture(t)
	ctx := context.Background()
	tgID := int64(999)
	pendingTxID := insertPending(t, store, "pend-stale", "2026-07-17", 4.07)
	msgID := int64(4242)
	if err := store.MarkTransactionNotified(ctx, sqlcgen.MarkTransactionNotifiedParams{
		TgMessageID: &msgID, TxID: pendingTxID,
	}); err != nil {
		t.Fatalf("mark notified: %v", err)
	}

	persistAdded(t, store, cfg, "c1", settledTx("post-stale", "", "2026-07-17", "", 4.07))

	row, err := store.GetTransaction(ctx, pendingTxID)
	if err != nil {
		t.Fatalf("load adopted row: %v", err)
	}
	if row.MessageStale != 1 {
		t.Errorf("message_stale = %d, want 1 after adoption", row.MessageStale)
	}
	if row.TgMessageID == nil || *row.TgMessageID != msgID {
		t.Errorf("tg_message_id = %v, want %d preserved through adoption", row.TgMessageID, msgID)
	}
	stale, err := store.ListStaleMessages(ctx, &tgID)
	if err != nil {
		t.Fatalf("list stale: %v", err)
	}
	if len(stale) != 1 || stale[0].Transaction.TxID != pendingTxID {
		t.Fatalf("stale list = %d rows, want the adopted row", len(stale))
	}
	if err := store.ClearMessageStale(ctx, pendingTxID); err != nil {
		t.Fatalf("clear stale: %v", err)
	}
	stale, err = store.ListStaleMessages(ctx, &tgID)
	if err != nil {
		t.Fatalf("list stale after clear: %v", err)
	}
	if len(stale) != 0 {
		t.Errorf("stale list should be empty after clear, got %d rows", len(stale))
	}
}

// TestMarkNotifiedKeepsMessageID: a failed re-send (nil message id) must not
// erase the card id recorded by the original announcement.
func TestMarkNotifiedKeepsMessageID(t *testing.T) {
	store, _ := newSyncFixture(t)
	ctx := context.Background()
	txID := insertPending(t, store, "pend-msg", "2026-07-17", 5.00)
	msgID := int64(77)
	if err := store.MarkTransactionNotified(ctx, sqlcgen.MarkTransactionNotifiedParams{TgMessageID: &msgID, TxID: txID}); err != nil {
		t.Fatalf("mark notified: %v", err)
	}
	if err := store.MarkTransactionNotified(ctx, sqlcgen.MarkTransactionNotifiedParams{TgMessageID: nil, TxID: txID}); err != nil {
		t.Fatalf("mark notified again: %v", err)
	}
	row, err := store.GetTransaction(ctx, txID)
	if err != nil {
		t.Fatalf("load row: %v", err)
	}
	if row.TgMessageID == nil || *row.TgMessageID != msgID {
		t.Errorf("tg_message_id = %v, want %d preserved", row.TgMessageID, msgID)
	}
}

// findByPlaidID scans the user's table rows for the one carrying plaidID.
func findByPlaidID(t *testing.T, store *database.Store, plaidID string) sqlcgen.Transaction {
	t.Helper()
	rows, err := store.ListTransactionsByUser(context.Background(), "u1")
	if err != nil {
		t.Fatalf("list transactions: %v", err)
	}
	for _, r := range rows {
		tx, err := store.GetTransaction(context.Background(), r.TxID)
		if err != nil {
			t.Fatalf("load %s: %v", r.TxID, err)
		}
		if tx.PlaidTxID == plaidID {
			return tx
		}
	}
	t.Fatalf("no row with plaid id %s", plaidID)
	return sqlcgen.Transaction{}
}

// TestTipRangeCandidateRecorded: a settled amount above the pending but within
// tip range must not auto-merge; it inserts with a merge offer pointing at the
// pending row, leaving the user to confirm.
func TestTipRangeCandidateRecorded(t *testing.T) {
	store, cfg := newSyncFixture(t)
	pendingTxID := insertPending(t, store, "pend-dinner", "2026-07-16", 20.00)

	persistAdded(t, store, cfg, "c1", settledTx("post-dinner", "pend-ghost", "2026-07-18", "2026-07-16", 24.00))

	settled := findByPlaidID(t, store, "post-dinner")
	if settled.TxID == pendingTxID {
		t.Fatal("tip-range settlement must not auto-merge")
	}
	if settled.MergeCandidateTxID == nil || *settled.MergeCandidateTxID != pendingTxID {
		t.Errorf("merge_candidate_tx_id = %v, want %s", settled.MergeCandidateTxID, pendingTxID)
	}
	pending, err := store.GetTransaction(context.Background(), pendingTxID)
	if err != nil {
		t.Fatalf("load pending: %v", err)
	}
	if pending.Pending != 1 {
		t.Errorf("pending row must stay unclaimed until the user confirms")
	}
}

// TestPreauthFallsOff: a $1-style preauth against a much larger settlement is
// no merge candidate; when Plaid removes the pending, its card is flagged so
// the sweep can strike it through.
func TestPreauthFallsOff(t *testing.T) {
	store, cfg := newSyncFixture(t)
	ctx := context.Background()
	pendingTxID := insertPending(t, store, "pend-hold", "2026-07-16", 1.00)
	msgID := int64(31337)
	if err := store.MarkTransactionNotified(ctx, sqlcgen.MarkTransactionNotifiedParams{TgMessageID: &msgID, TxID: pendingTxID}); err != nil {
		t.Fatalf("mark notified: %v", err)
	}

	persistAdded(t, store, cfg, "c1", settledTx("post-gas", "", "2026-07-18", "2026-07-16", 45.00))

	settled := findByPlaidID(t, store, "post-gas")
	if settled.MergeCandidateTxID != nil {
		t.Errorf("preauth must not be offered as a merge candidate, got %v", settled.MergeCandidateTxID)
	}
	if err := store.SoftDeleteTransaction(ctx, "pend-hold"); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	row, err := store.GetTransaction(ctx, pendingTxID)
	if err != nil {
		t.Fatalf("load removed pending: %v", err)
	}
	if row.RemovedAt == nil || row.MessageStale != 1 {
		t.Errorf("fell-off pending should be removed and card-flagged, got removed_at=%v stale=%d", row.RemovedAt, row.MessageStale)
	}
	tgID := int64(999)
	stale, err := store.ListStaleMessages(ctx, &tgID)
	if err != nil {
		t.Fatalf("list stale: %v", err)
	}
	if len(stale) != 1 || stale[0].Transaction.TxID != pendingTxID {
		t.Fatalf("stale list should surface the fell-off pending, got %d rows", len(stale))
	}
}

// TestMergeSettledIntoPending exercises the confirmed-merge fold the way
// handleMerge runs it: delete the settled row, then move its identity and
// tip-inclusive amount onto the pending row inside one database transaction.
func TestMergeSettledIntoPending(t *testing.T) {
	store, cfg := newSyncFixture(t)
	ctx := context.Background()
	pendingTxID := insertPending(t, store, "pend-tip", "2026-07-16", 20.00)
	if err := store.SetAmortEnd(ctx, sqlcgen.SetAmortEndParams{Modifier: "+7 days", TxID: pendingTxID}); err != nil {
		t.Fatalf("set amort: %v", err)
	}
	persistAdded(t, store, cfg, "c1", settledTx("post-tip", "", "2026-07-18", "2026-07-16", 24.00))
	settled := findByPlaidID(t, store, "post-tip")

	err := store.ExecTx(ctx, func(q *sqlcgen.Queries) error {
		if err := q.DeleteTransactionByTxID(ctx, settled.TxID); err != nil {
			return err
		}
		res, err := q.MergeSettledIntoPending(ctx, sqlcgen.MergeSettledIntoPendingParams{
			PostedPlaidID:      settled.PlaidTxID,
			Date:               settled.Date,
			AuthorizedDate:     settled.AuthorizedDate,
			Amount:             settled.Amount,
			Name:               settled.Name,
			MerchantName:       settled.MerchantName,
			CategoryPrimary:    settled.CategoryPrimary,
			CategoryDetailed:   settled.CategoryDetailed,
			CategoryConfidence: settled.CategoryConfidence,
			PaymentChannel:     settled.PaymentChannel,
			RawJson:            settled.RawJson,
			PendingTxID:        pendingTxID,
		})
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n != 1 {
			t.Fatalf("merge matched %d rows, err %v", n, err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("merge tx: %v", err)
	}

	row, err := store.GetTransaction(ctx, pendingTxID)
	if err != nil {
		t.Fatalf("load merged row: %v", err)
	}
	if row.PlaidTxID != "post-tip" || row.Pending != 0 || row.Amount != 24.00 {
		t.Errorf("merged row wrong: plaid=%q pending=%d amount=%v", row.PlaidTxID, row.Pending, row.Amount)
	}
	if row.AmortEnd == nil || *row.AmortEnd != "2026-07-23" {
		t.Errorf("amort_end = %v, want preserved 2026-07-23", row.AmortEnd)
	}
	if row.MessageStale != 1 {
		t.Errorf("merged row should flag its card for refresh")
	}
	if n := countTransactions(t, store); n != 1 {
		t.Errorf("transaction count = %d, want 1 after merge", n)
	}
}

// TestExcludedCardRendersStruckThrough: flipping exclusion strikes the card
// body through and notes it; flipping back restores the plain render.
func TestExcludedCardRendersStruckThrough(t *testing.T) {
	tx := sqlcgen.Transaction{Amount: 12.34, Name: "Test Merchant", Date: "2026-07-16"}
	plain := formatTransactionMessage(tx)
	if strings.Contains(plain, "<s>") || strings.Contains(plain, "excluded from spend") {
		t.Errorf("included card must not be struck through: %q", plain)
	}
	tx.Excluded = 1
	struck := formatTransactionMessage(tx)
	if !strings.HasPrefix(struck, "<s>") || !strings.Contains(struck, "</s>\n🚫 <i>excluded from spend</i>") {
		t.Errorf("excluded card should strike the body and note exclusion: %q", struck)
	}
}

func countTransactions(t *testing.T, store *database.Store) int {
	t.Helper()
	rows, err := store.ListTransactionsByUser(context.Background(), "u1")
	if err != nil {
		t.Fatalf("list transactions: %v", err)
	}
	return len(rows)
}
