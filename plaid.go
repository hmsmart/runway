package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hmsmart/runway/database"
	"github.com/hmsmart/runway/database/sqlcgen"
	"github.com/plaid/plaid-go/v43/plaid"
)

type TransactionNotifier func(ctx context.Context, tx sqlcgen.UpsertTransactionParams)

// inFlightSyncs prevents the same item from syncing concurrently, e.g. a
// link request arriving while the startup sweep is still running.
var inFlightSyncs sync.Map

// syncItem is the single entry point for all sync triggers: it refreshes the
// item's accounts, then pulls transactions from the given cursor. Accounts go
// first so transaction rows never hit a missing account_id foreign key.
func syncItem(ctx context.Context, itemID string, accessToken string, cursor *string, plaidClient *plaid.APIClient, store *database.Store, cfg *Config, notify TransactionNotifier) error {
	if _, busy := inFlightSyncs.LoadOrStore(itemID, struct{}{}); busy {
		slog.Info("sync already in progress, skipping", "item", itemID)
		return nil
	}
	defer inFlightSyncs.Delete(itemID)
	if err := syncAllAccounts(ctx, itemID, accessToken, plaidClient, store, cfg); err != nil {
		return fmt.Errorf("sync accounts: %w", err)
	}
	if err := syncTransactions(ctx, itemID, accessToken, cursor, plaidClient, store, cfg, notify); err != nil {
		return fmt.Errorf("sync transactions: %w", err)
	}
	return nil
}

func syncTransactions(ctx context.Context, itemID string, accessToken string, cursor *string, plaidClient *plaid.APIClient, store *database.Store, cfg *Config, notify TransactionNotifier) error {
	var added, modified, removed int
	for hasMore := true; hasMore; {
		resp, err := fetchTransactionsPage(ctx, accessToken, cursor, plaidClient, cfg)
		if err != nil {
			slog.Error("failed to retrieve transactions", "cursor", cursorValue(cursor), "item", itemID, "err", err)
			return err
		}
		nextCursor := resp.GetNextCursor()
		if err := persistTransactionsPage(ctx, itemID, resp, nextCursor, store, cfg, notify); err != nil {
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
// from the last completed page instead of re-downloading everything.

func persistTransactionsPage(ctx context.Context, itemID string, resp plaid.TransactionsSyncResponse, nextCursor string, store *database.Store, cfg *Config, notify TransactionNotifier) error {
	now := time.Now()
	return store.ExecTx(ctx, func(q *sqlcgen.Queries) error {
		for _, tx := range resp.GetAdded() {
			params := transactionParams(tx, cfg)
			if err := q.UpsertTransaction(ctx, params); err != nil {
				return fmt.Errorf("upsert transaction %s: %w", tx.GetTransactionId(), err)
			}
			notify(ctx, params)
		}
		for _, tx := range resp.GetModified() {
			params := transactionParams(tx, cfg)
			if err := q.UpsertTransaction(ctx, transactionParams(tx, cfg)); err != nil {
				return fmt.Errorf("upsert transaction %s: %w", tx.GetTransactionId(), err)
			}
			notify(ctx, params)
		}
		for _, tx := range resp.GetRemoved() {
			if err := q.SoftDeleteTransaction(ctx, tx.GetTransactionId()); err != nil {
				return fmt.Errorf("delete transaction %s: %w", tx.GetTransactionId(), err)
			}
		}
		return q.UpdateItemCursor(ctx, sqlcgen.UpdateItemCursorParams{
			Cursor:       sql.NullString{String: nextCursor, Valid: true},
			LastSyncedAt: sql.NullTime{Time: now, Valid: true},
			ItemID:       itemID,
		})
	})
}

func transactionParams(tx plaid.Transaction, cfg *Config) sqlcgen.UpsertTransactionParams {
	var pending int64
	if tx.GetPending() {
		pending = 1
	}
	var catPrimary, catDetailed sql.NullString
	if pfc, ok := tx.GetPersonalFinanceCategoryOk(); ok {
		catPrimary = ToNullString(&pfc.Primary, true)
		catDetailed = ToNullString(&pfc.Detailed, true)
	}
	txid, err := uuid.NewV7()
	if err != nil {
		panic("uuid " + err.Error())
	}
	return sqlcgen.UpsertTransactionParams{
		TxID:             txid.String(),
		PlaidTxID:        tx.GetTransactionId(),
		AccountID:        tx.GetAccountId(),
		Date:             tx.GetDate(),
		Amount:           tx.GetAmount(),
		Name:             ToNullString(tx.GetNameOk()),
		MerchantName:     ToNullString(tx.GetMerchantNameOk()),
		CategoryPrimary:  catPrimary,
		CategoryDetailed: catDetailed,
		PaymentChannel:   ToNullString(tx.GetPaymentChannelOk()),
		Pending:          pending,
		RawJson:          encryptedRawJSON(tx, tx.GetTransactionId(), cfg.DBCryptKey),
	}
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
	now := time.Now()
	accounts := accountsGetResp.GetAccounts()
	var operations = []sqlcgen.UpsertAccountParams{}
	for _, pa := range accounts {
		accID, valid := pa.GetAccountIdOk()
		if !valid {
			slog.Warn("discarding account with invalid account id", "item", itemID)
			continue
		}
		accName, valid := pa.GetNameOk()
		if !valid {
			slog.Warn("discarding account with invalid account name", "account", *accID, "item", itemID)
			continue
		}
		op := sqlcgen.UpsertAccountParams{
			ItemID:           itemID,
			AccountID:        *accID,
			Name:             *accName,
			Mask:             ToNullString(pa.GetMaskOk()),
			Type:             ToNullString(pa.GetTypeOk()),
			Subtype:          ToNullString(pa.GetSubtypeOk()),
			BalanceAvailable: ToNullFloat64(pa.Balances.GetAvailableOk()),
			BalanceCurrent:   ToNullFloat64(pa.Balances.GetCurrentOk()),
			Tracked:          1, // applies to new rows only; the upsert preserves the user's toggle on conflict
			IsoCurrencyCode:  ToNullString(pa.Balances.GetIsoCurrencyCodeOk()),
			LastSyncedAt:     sql.NullTime{Time: now, Valid: true},
			RawJson:          encryptedRawJSON(pa, *accID, cfg.DBCryptKey),
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
func encryptedRawJSON(v json.Marshaler, id string, key []byte) sql.NullString {
	raw, err := v.MarshalJSON()
	if err != nil {
		slog.Warn("discarding raw json, marshal failed", "id", id, "err", err)
		return sql.NullString{}
	}
	enc, err := EncryptColumnSecret(string(raw), id, key)
	if err != nil {
		slog.Warn("discarding raw json, encryption failed", "id", id, "err", err)
		return sql.NullString{}
	}
	return sql.NullString{String: enc, Valid: true}
}

func cursorValue(cursor *string) string {
	if cursor == nil {
		return ""
	}
	return *cursor
}
