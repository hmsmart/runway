package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hmsmart/runway/database"
	"github.com/hmsmart/runway/database/sqlcgen"
	"github.com/hmsmart/runway/domains"
)

type holdPayload struct {
	Amount   string `json:"Amount"`
	Merchant string `json:"Merchant"`
	Name     string `json:"Name"`
	CardPass string `json:"CardPass"`
}

func handleHoldWebhook(store *database.Store, cfg *Config, tg *TelegramBot) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		var payload holdPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			slog.Warn("hold webhook: bad json", "for", ip, "err", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		apiKey := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		apiKey = strings.TrimSpace(apiKey)
		if apiKey == "" {
			slog.Warn("hold webhook: missing Authorization header", "for", ip)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		usql, err := store.GetUserByAPIKey(r.Context(), &apiKey)
		if err != nil {
			slog.Warn("hold webhook: invalid api key", "for", ip)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		u := domains.NewUser(usql)
		if u == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		amt, err := parseHoldAmount(payload.Amount)
		if err != nil {
			slog.Warn("hold webhook: bad amount", "for", ip, "raw", payload.Amount, "err", err)
			http.Error(w, "bad amount", http.StatusBadRequest)
			return
		}

		merchant := strings.TrimSpace(payload.Merchant)
		if merchant == "" {
			merchant = strings.TrimSpace(payload.Name)
		}
		if merchant == "" {
			merchant = "Card Transaction"
		}

		txID, err := createHoldTransaction(r.Context(), store, cfg, u.ID(), amt, merchant)
		if err != nil {
			slog.Error("hold webhook: failed to create hold", "user", u.ID(), "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		slog.Info("hold webhook: created hold", "user", u.ID(), "tx", txID, "amt", amt, "merchant", merchant, "name", payload.Name, "cardpass", payload.CardPass)
		if err := recomputeDailySpend(r.Context(), store, u.ID()); err != nil {
			slog.Error("hold webhook: failed to recompute daily spend", "user", u.ID(), "err", err)
		}
		tg.startDrain(u.TelegramID())
		w.WriteHeader(http.StatusOK)
	}
}

func parseHoldAmount(raw string) (float64, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.ReplaceAll(raw, "$", "")
	raw = strings.ReplaceAll(raw, ",", "")
	var amt float64
	if _, err := fmt.Sscanf(raw, "%f", &amt); err != nil {
		return 0, fmt.Errorf("parse amount %q: %w", raw, err)
	}
	amt = math.Floor(amt*100) / 100
	if !(amt > 0 && amt < 1_000_000) {
		return 0, fmt.Errorf("amount out of range: %v", amt)
	}
	return amt, nil
}

func createHoldTransaction(ctx context.Context, store *database.Store, cfg *Config, userID string, amount float64, merchant string) (string, error) {
	accountID, err := ensureManualAccount(ctx, store, userID)
	if err != nil {
		return "", err
	}
	txid, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("generate tx uuid: %w", err)
	}
	plaidTxID := "hold:" + txid.String()
	today := time.Now().Format(time.DateOnly)
	_, err = store.UpsertTransaction(ctx, sqlcgen.UpsertTransactionParams{
		TxID:             txid.String(),
		PlaidTxID:        plaidTxID,
		AccountID:        accountID,
		Date:             today,
		AuthorizedDate:   &today,
		Amount:           amount,
		Name:             merchant,
		MerchantName:     &merchant,
		CategoryPrimary:  "GENERAL_MERCHANDISE",
		CategoryDetailed: "GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE",
		PaymentChannel:   "in store",
		Pending:          1,
		Notified:         0,
	})
	if err != nil {
		return "", fmt.Errorf("insert hold transaction: %w", err)
	}
	return txid.String(), nil
}

func holdAdoptParams(p sqlcgen.UpsertTransactionParams, userID string) sqlcgen.AdoptHoldByAmountParams {
	return sqlcgen.AdoptHoldByAmountParams{
		PostedPlaidID:      p.PlaidTxID,
		AccountID:          p.AccountID,
		Date:               p.Date,
		AuthorizedDate:     p.AuthorizedDate,
		Amount:             p.Amount,
		Name:               p.Name,
		MerchantName:       p.MerchantName,
		CategoryPrimary:    p.CategoryPrimary,
		CategoryDetailed:   p.CategoryDetailed,
		CategoryConfidence: p.CategoryConfidence,
		PaymentChannel:     p.PaymentChannel,
		LogoUrl:            p.LogoUrl,
		RawJson:            p.RawJson,
		UserID:             userID,
		EffectiveDate:      effectiveDate(p),
	}
}

