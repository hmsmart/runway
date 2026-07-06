package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/hmsmart/runway/database"
	"github.com/hmsmart/runway/database/sqlcgen"
	"github.com/plaid/plaid-go/v43/plaid"
)

func syncTranscations(ctx context.Context, itemId string, accessToken string, curCursor *string, plaidClient *plaid.APIClient, store *database.Store, cfg *Config) error {
	// Provide a cursor from your database if you've previously
	// received one for the Item. Leave null if this is your
	// first sync call for this Item. The first request will
	// return a cursor.
	cursor := curCursor

	// New transaction updates since "cursor"
	var added []plaid.Transaction
	var modified []plaid.Transaction
	var removed []plaid.RemovedTransaction // Removed transaction ids
	hasMore := true

	// Iterate through each page of new transaction updates for item
	for hasMore {
		request := plaid.NewTransactionsSyncRequest(accessToken)
		if cursor != nil {
			request.SetCursor(*cursor)
		}
		callCtx, cancel := context.WithTimeout(ctx, cfg.PlaidTimeout)
		defer cancel()
		resp, _, err := plaidClient.PlaidApi.TransactionsSync(
			callCtx,
		).TransactionsSyncRequest(*request).Execute()

		if err != nil {
			slog.Error("failed to retrieve transactions", "cursor", cursor, "item", itemId, "err", err)
			return err
		}

		// Add this page of results
		added = append(added, resp.GetAdded()...)
		modified = append(modified, resp.GetModified()...)
		removed = append(removed, resp.GetRemoved()...)

		hasMore = resp.GetHasMore()

		// Update cursor to the next cursor
		nextCursor := resp.GetNextCursor()
		cursor = &nextCursor
	}
	//Prep to Batch
	slog.Info("got transactions", "item", itemId, "add", len(added), "mod", len(modified), "rem", len(removed))
	return nil
}
func syncAllAccounts(ctx context.Context, itemId string, plaidClient *plaid.APIClient, store *database.Store, cfg *Config) error {
	item, err := store.GetItemByID(ctx, itemId)
	if err != nil {
		return fmt.Errorf("unable to query for item: %w", err)
	}
	ptAtoken, err := DecryptColumnSecret(item.AccessToken, itemId, cfg.DBCryptKey)
	if err != nil {
		return fmt.Errorf("unable to decrypt secret: %w", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, cfg.PlaidTimeout)
	defer cancel()
	accountsGetRequest := plaid.NewAccountsGetRequest(ptAtoken)
	accountsGetResp, _, err := plaidClient.PlaidApi.AccountsGet(callCtx).AccountsGetRequest(
		*accountsGetRequest,
	).Execute()
	if err != nil {
		return fmt.Errorf("unable to request accounts from api: %w", err)
	}
	accounts := accountsGetResp.GetAccounts()
	var operations = []sqlcgen.UpsertAccountParams{}
	for _, pa := range accounts {
		accID, valid := pa.GetAccountIdOk()
		if !valid {
			slog.Warn("disacarding account with invalid account id", "item", item)
			continue
		}
		accName, valid := pa.GetNameOk()
		if !valid {
			slog.Warn("discarding account with invalid account name", "account", accID, "item", item)
			continue
		}
		var nencJ sql.NullString
		rawJ, err := pa.MarshalJSON()
		if err != nil {
			slog.Warn("discarding raw json, marshal failed",
				"account", accID, "item", item)
		} else {
			enc, err := EncryptColumnSecret(string(rawJ), *accID, cfg.DBCryptKey)
			if err != nil {
				slog.Warn("discarding raw json, encryption failed",
					"account", accID, "item", item)
			} else {
				nencJ = ToNullString(&enc, true)
			}
		}
		op := sqlcgen.UpsertAccountParams{
			ItemID:           itemId,
			AccountID:        *accID,
			Name:             *accName,
			Mask:             ToNullString(pa.GetMaskOk()),
			Type:             ToNullString(pa.GetTypeOk()),
			Subtype:          ToNullString(pa.GetSubtypeOk()),
			BalanceAvailable: ToNullFloat64(pa.Balances.GetAvailableOk()),
			BalanceCurrent:   ToNullFloat64(pa.Balances.GetCurrentOk()),
			Tracked:          1,
			IsoCurrencyCode:  ToNullString(pa.Balances.GetIsoCurrencyCodeOk()),
			RawJson:          nencJ,
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
				slog.Error("failed to update account", "account", op.AccountID, "item", op.ItemID, "name", op.Name, "err", err)
				return err
			}
		}
		return nil
	}
	return store.ExecTx(ctx, transaction)
}
