package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hmsmart/runway/database"
	"github.com/hmsmart/runway/database/sqlcgen"
	"github.com/plaid/plaid-go/v43/plaid"
)

// inFlightSyncs prevents the same item from syncing concurrently, e.g. a
// link request arriving while the startup sweep is still running.
var inFlightSyncs sync.Map

// syncItem is the single entry point for all sync triggers: it refreshes the
// item's accounts, then pulls transactions from the given cursor. Accounts go
// first so transaction rows never hit a missing account_id foreign key.
// Callers kick the owning chat's notification drain after this returns;
// freshly inserted rows sit at notified = 0 until the drain sends them.
func syncItem(ctx context.Context, itemID string, accessToken string, cursor *string, plaidClient *plaid.APIClient, store *database.Store, cfg *Config) error {
	if _, busy := inFlightSyncs.LoadOrStore(itemID, struct{}{}); busy {
		slog.Info("sync already in progress, skipping", "item", itemID)
		return nil
	}
	defer inFlightSyncs.Delete(itemID)
	if err := syncAllAccounts(ctx, itemID, accessToken, plaidClient, store, cfg); err != nil {
		return fmt.Errorf("sync accounts: %w", err)
	}
	if err := syncTransactions(ctx, itemID, accessToken, cursor, plaidClient, store, cfg); err != nil {
		return fmt.Errorf("sync transactions: %w", err)
	}
	// The daily-spend series derives from transactions, so refresh it after
	// every sync, whichever trigger (webhook, startup sweep, post-link) fired.
	item, err := store.GetItemByID(ctx, itemID)
	if err != nil {
		return fmt.Errorf("load item for spend recompute: %w", err)
	}
	if err := recomputeDailySpend(ctx, store, item.UserID); err != nil {
		return fmt.Errorf("recompute daily spend: %w", err)
	}
	return nil
}

func syncTransactions(ctx context.Context, itemID string, accessToken string, cursor *string, plaidClient *plaid.APIClient, store *database.Store, cfg *Config) error {
	// The 30-day backfill is a first-account-only courtesy: an initial pull
	// (nil cursor) for any later item inserts its history already notified,
	// so linking another bank never floods the chat. Deciding at insert time
	// closes the race where a webhook kicks the drain mid-pull.
	var notified int64
	if cursor == nil {
		suppress, err := suppressBackfill(ctx, itemID, store)
		if err != nil {
			return err
		}
		if suppress {
			notified = 1
		}
	}
	var added, modified, removed int
	for hasMore := true; hasMore; {
		resp, err := fetchTransactionsPage(ctx, accessToken, cursor, plaidClient, cfg)
		if err != nil {
			slog.Error("failed to retrieve transactions", "cursor", cursorValue(cursor), "item", itemID, "err", err)
			return err
		}
		nextCursor := resp.GetNextCursor()
		if err := persistTransactionsPage(ctx, itemID, resp, nextCursor, store, cfg, notified); err != nil {
			slog.Error("failed to persist transactions", "cursor", cursorValue(cursor), "item", itemID, "err", err)
			return err
		}
		added += len(resp.GetAdded())
		modified += len(resp.GetModified())
		removed += len(resp.GetRemoved())
		hasMore = resp.GetHasMore()
		cursor = &nextCursor
	}
	slog.Info("transaction sync complete", "item", itemID, "added", added, "modified", modified, "removed", removed)
	return nil
}

func fetchTransactionsPage(ctx context.Context, accessToken string, cursor *string, plaidClient *plaid.APIClient, cfg *Config) (plaid.TransactionsSyncResponse, error) {
	callCtx, cancel := context.WithTimeout(ctx, cfg.PlaidTimeout)
	defer cancel()
	request := plaid.NewTransactionsSyncRequest(accessToken)
	if cursor != nil {
		request.SetCursor(*cursor)
	}
	resp, _, err := plaidClient.PlaidApi.TransactionsSync(callCtx).TransactionsSyncRequest(*request).Execute()
	return resp, err
}

// persistTransactionsPage writes one page of sync results and advances the
// item's cursor in the same database transaction, so a crash mid-sync resumes
// from the last completed page instead of re-downloading everything. Fresh
// inserts land at notified = 0 for the drain worker; conflict updates leave
// the flag alone so re-synced rows never re-announce.
func persistTransactionsPage(ctx context.Context, itemID string, resp plaid.TransactionsSyncResponse, nextCursor string, store *database.Store, cfg *Config, notified int64) error {
	now := time.Now()
	return store.ExecTx(ctx, func(q *sqlcgen.Queries) error {
		for _, tx := range slices.Concat(resp.GetAdded(), resp.GetModified()) {
			params, err := transactionParams(tx, cfg, notified)
			if err != nil {
				return err
			}
			// A settled transaction that names its pending predecessor adopts
			// that row in place (see AdoptPendingTransaction) instead of
			// inserting a duplicate; the later "removed" event for the pending
			// id then no-ops. Zero rows adopted (no pending sibling, or the
			// posted id already has a row) falls through to the plain upsert.
			if pid, ok := tx.GetPendingTransactionIdOk(); ok && pid != nil && *pid != "" {
				res, err := q.AdoptPendingTransaction(ctx, adoptParams(params, *pid))
				if err != nil {
					return fmt.Errorf("adopt pending transaction %s: %w", *pid, err)
				}
				if n, err := res.RowsAffected(); err == nil && n == 1 {
					slog.Info("adopted pending transaction", "pending", *pid, "posted", params.PlaidTxID)
					continue
				}
			}
			if _, err := q.UpsertTransaction(ctx, params); err != nil {
				return fmt.Errorf("upsert transaction %s: %w", tx.GetTransactionId(), err)
			}
			// Replay recovery: the posted row existed (so adoption skipped),
			// Plaid sent no authorized date, but the pending sibling may
			// still hold the real transaction date.
			if pid, ok := tx.GetPendingTransactionIdOk(); ok && pid != nil && *pid != "" && params.AuthorizedDate == nil {
				err := q.BackfillAuthorizedDate(ctx, sqlcgen.BackfillAuthorizedDateParams{
					PendingPlaidID: *pid,
					PostedPlaidID:  params.PlaidTxID,
				})
				if err != nil {
					return fmt.Errorf("backfill authorized date %s: %w", params.PlaidTxID, err)
				}
			}
		}
		for _, tx := range resp.GetRemoved() {
			if err := q.SoftDeleteTransaction(ctx, tx.GetTransactionId()); err != nil {
				return fmt.Errorf("delete transaction %s: %w", tx.GetTransactionId(), err)
			}
		}
		return q.UpdateItemCursor(ctx, sqlcgen.UpdateItemCursorParams{
			Cursor:       &nextCursor,
			LastSyncedAt: &now,
			ItemID:       itemID,
		})
	})
}

// suppressBackfill reports whether an initial history pull for this item
// should skip notifications: only the user's first account gets the backfill.
func suppressBackfill(ctx context.Context, itemID string, store *database.Store) (bool, error) {
	item, err := store.GetItemByID(ctx, itemID)
	if err != nil {
		return false, fmt.Errorf("load item for backfill check: %w", err)
	}
	n, err := store.CountItemsByUser(ctx, item.UserID)
	if err != nil {
		return false, fmt.Errorf("count user items for backfill check: %w", err)
	}
	return n > 1, nil
}

// adoptParams maps an upsert's fields onto the adopt query, plus the pending
// row being claimed.
func adoptParams(p sqlcgen.UpsertTransactionParams, pendingPlaidID string) sqlcgen.AdoptPendingTransactionParams {
	return sqlcgen.AdoptPendingTransactionParams{
		PostedPlaidID:      p.PlaidTxID,
		Date:               p.Date,
		AuthorizedDate:     p.AuthorizedDate,
		Amount:             p.Amount,
		Name:               p.Name,
		MerchantName:       p.MerchantName,
		CategoryPrimary:    p.CategoryPrimary,
		CategoryDetailed:   p.CategoryDetailed,
		CategoryConfidence: p.CategoryConfidence,
		PaymentChannel:     p.PaymentChannel,
		RawJson:            p.RawJson,
		PendingPlaidID:     pendingPlaidID,
	}
}

func transactionParams(tx plaid.Transaction, cfg *Config, notified int64) (sqlcgen.UpsertTransactionParams, error) {
	var pending int64
	if tx.GetPending() {
		pending = 1
	}
	txid, err := uuid.NewV7()
	if err != nil {
		return sqlcgen.UpsertTransactionParams{}, fmt.Errorf("generate tx uuid: %w", err)
	}
	return sqlcgen.UpsertTransactionParams{
		TxID:               txid.String(),
		PlaidTxID:          tx.GetTransactionId(),
		AccountID:          tx.GetAccountId(),
		Date:               tx.GetDate(),
		AuthorizedDate:     StringPtrOk(tx.GetAuthorizedDateOk()),
		Amount:             tx.GetAmount(),
		Name:               tx.GetName(),
		MerchantName:       tx.MerchantName.Get(),
		CategoryPrimary:    tx.GetPersonalFinanceCategory().Primary,
		CategoryDetailed:   tx.GetPersonalFinanceCategory().Detailed,
		CategoryConfidence: tx.GetPersonalFinanceCategory().ConfidenceLevel.Get(),
		PaymentChannel:     tx.GetPaymentChannel(),
		Pending:            pending,
		Notified:           notified,
		RawJson:            encryptedRawJSON(tx, tx.GetTransactionId(), cfg.DBCryptKey),
	}, nil
}

func syncAllAccounts(ctx context.Context, itemID string, accessToken string, plaidClient *plaid.APIClient, store *database.Store, cfg *Config) error {
	callCtx, cancel := context.WithTimeout(ctx, cfg.PlaidTimeout)
	defer cancel()
	accountsGetRequest := plaid.NewAccountsGetRequest(accessToken)
	accountsGetResp, _, err := plaidClient.PlaidApi.AccountsGet(callCtx).AccountsGetRequest(
		*accountsGetRequest,
	).Execute()
	if err != nil {
		return fmt.Errorf("unable to request accounts from api: %w", err)
	}
	accounts := accountsGetResp.GetAccounts()
	// last_synced_at records when we synced, not the institution-side balance
	// timestamp — Plaid omits that for most institutions, and each AccountsGet
	// response is current by definition. The raw payload keeps Plaid's value.
	now := time.Now()
	var operations = []sqlcgen.UpsertAccountParams{}
	for _, pa := range accounts {
		op := sqlcgen.UpsertAccountParams{
			ItemID:           itemID,
			AccountID:        pa.AccountId,
			Name:             pa.Name,
			Mask:             pa.Mask.Get(),
			Type:             (*string)(pa.GetType().Ptr()),
			Subtype:          (*string)(pa.GetSubtype().Ptr()),
			BalanceAvailable: pa.GetBalances().Available.Get(),
			BalanceCurrent:   pa.GetBalances().Current.Get(),
			Tracked:          1, // applies to new rows only; the upsert preserves the user's toggle on conflict
			IsoCurrencyCode:  pa.GetBalances().IsoCurrencyCode.Get(),
			LastSyncedAt:     &now,
			RawJson:          encryptedRawJSON(pa, pa.AccountId, cfg.DBCryptKey),
		}
		operations = append(operations, op)
	}
	if len(operations) == 0 {
		return nil
	}
	transaction := func(q *sqlcgen.Queries) error {
		for _, op := range operations {
			err := q.UpsertAccount(ctx, op)
			if err != nil {
				slog.Error("failed to update account", "account", op.AccountID, "item", op.ItemID, "err", err)
				return err
			}
		}
		return nil
	}
	return store.ExecTx(ctx, transaction)
}

// encryptedRawJSON marshals v and encrypts it for at-rest storage; on failure
// the raw payload is dropped (NULL) rather than failing the whole sync.
func encryptedRawJSON(v json.Marshaler, id string, key []byte) *string {
	raw, err := v.MarshalJSON()
	if err != nil {
		slog.Warn("discarding raw json, marshal failed", "id", id, "err", err)
		return nil
	}
	enc, err := EncryptColumnSecret(string(raw), id, key)
	if err != nil {
		slog.Warn("discarding raw json, encryption failed", "id", id, "err", err)
		return nil
	}
	return &enc
}

func cursorValue(cursor *string) string {
	if cursor == nil {
		return ""
	}
	return *cursor
}
