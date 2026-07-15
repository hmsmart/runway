package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hmsmart/runway/database"
	"github.com/hmsmart/runway/database/sqlcgen"
)

// receiptTransaction is the JSON object the vision model is prompted to
// return for a receipt.
type receiptTransaction struct {
	Merchant         string  `json:"merchant"`
	Amount           float64 `json:"amount"`
	CategoryPrimary  string  `json:"primary_category"`
	CategoryDetailed string  `json:"detailed_category"`
}

// parseReceiptTransaction decodes and sanity-checks the model's JSON. The
// model is prompted with an allowed-category list but is still a language
// model, so nothing it produced is trusted: the amount gets the same range
// check as user-typed budgets and the categories must look like the
// PRIMARY/PRIMARY_DETAIL pairs the rest of the app renders.
func parseReceiptTransaction(raw string) (receiptTransaction, error) {
	var rt receiptTransaction
	if err := json.Unmarshal([]byte(raw), &rt); err != nil {
		return receiptTransaction{}, fmt.Errorf("decode receipt json: %w", err)
	}
	rt.Merchant = strings.TrimSpace(rt.Merchant)
	if rt.Merchant == "" {
		return receiptTransaction{}, errors.New("receipt json has no merchant")
	}
	rt.Amount = math.Floor(rt.Amount*100) / 100
	if !(rt.Amount > 0 && rt.Amount < 1_000_000) {
		return receiptTransaction{}, fmt.Errorf("receipt amount out of range: %v", rt.Amount)
	}
	if _, ok := categoryDisplay[rt.CategoryPrimary]; !ok {
		return receiptTransaction{}, fmt.Errorf("unknown primary category %q", rt.CategoryPrimary)
	}
	if !strings.HasPrefix(rt.CategoryDetailed, rt.CategoryPrimary+"_") {
		return receiptTransaction{}, fmt.Errorf("detailed category %q not under primary %q", rt.CategoryDetailed, rt.CategoryPrimary)
	}
	return rt, nil
}

// manualContainerID is both the item_id and account_id of a user's manual
// container (they live in different tables, so sharing the string is fine).
func manualContainerID(userID string) string {
	return "manual:" + userID
}

// ensureManualAccount returns the account_id manual transactions attach to,
// creating the hidden item+account pair on first use. The item's 'manual'
// status keeps it out of every status='active' path — the Plaid sync loop,
// /accounts, /unlink — while the account stays tracked so its transactions
// count toward daily spend. Nil balances mean it contributes nothing to
// /runway's cash-on-hand math.
func ensureManualAccount(ctx context.Context, store *database.Store, userID string) (string, error) {
	id := manualContainerID(userID)
	_, err := store.GetManualItemByUser(ctx, userID)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("look up manual item: %w", err)
	}
	name := "Manual"
	err = store.ExecTx(ctx, func(q *sqlcgen.Queries) error {
		if err := q.CreateItem(ctx, sqlcgen.CreateItemParams{
			ItemID:          id,
			UserID:          userID,
			AccessToken:     "",
			InstitutionName: &name,
			Status:          "manual",
		}); err != nil {
			return fmt.Errorf("create manual item: %w", err)
		}
		if err := q.UpsertAccount(ctx, sqlcgen.UpsertAccountParams{
			AccountID: id,
			ItemID:    id,
			Name:      "Manual receipts",
			Tracked:   1,
		}); err != nil {
			return fmt.Errorf("create manual account: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// createManualTransaction inserts a receipt-derived transaction through the
// same upsert the Plaid sync uses, so everything downstream — the drain
// worker's chat notification with Spread/Exclude buttons, daily-spend
// recomputes, the dashboard — treats it like any other transaction. The
// synthetic plaid_tx_id satisfies that column's NOT NULL UNIQUE and can never
// collide with a real Plaid id. Inserted at notified = 0; the caller kicks
// the drain worker to announce it.
func createManualTransaction(ctx context.Context, store *database.Store, cfg *Config, userID string, rt receiptTransaction, rawJSON string) (string, error) {
	accountID, err := ensureManualAccount(ctx, store, userID)
	if err != nil {
		return "", err
	}
	txid, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("generate tx uuid: %w", err)
	}
	plaidTxID := "manual:" + txid.String()
	var raw *string
	if enc, err := EncryptColumnSecret(rawJSON, plaidTxID, cfg.DBCryptKey); err == nil {
		raw = &enc
	}
	_, err = store.UpsertTransaction(ctx, sqlcgen.UpsertTransactionParams{
		TxID:             txid.String(),
		PlaidTxID:        plaidTxID,
		AccountID:        accountID,
		Date:             time.Now().Format(time.DateOnly),
		Amount:           rt.Amount,
		Name:             rt.Merchant,
		MerchantName:     &rt.Merchant,
		CategoryPrimary:  rt.CategoryPrimary,
		CategoryDetailed: rt.CategoryDetailed,
		PaymentChannel:   "manual",
		Pending:          0,
		Notified:         0,
		RawJson:          raw,
	})
	if err != nil {
		return "", fmt.Errorf("insert manual transaction: %w", err)
	}
	return txid.String(), nil
}
