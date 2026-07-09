package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"

	"github.com/hmsmart/runway/database"
	"github.com/hmsmart/runway/database/sqlcgen"
	"github.com/hmsmart/runway/templates"
	"github.com/plaid/plaid-go/v43/plaid"
)

func clientIP(r *http.Request) string {
	if xri := r.Header.Get("X-Real-Ip"); xri != "" {
		return xri
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func handleHealthz(store *database.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := store.Ping(r.Context()); err != nil {
			http.Error(w, "db unreachable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}
}
func handleTokenExchange(plaidClient *plaid.APIClient, store *database.Store, cfg *Config, notify TransactionNotifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		slog.Info("unwrapping pubtoken exchange request", "for", ip)
		var body struct {
			PublicToken string `json:"public_token"`
			LinkToken   string `json:"link_token"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			slog.Error("parsing body request", "for", ip, "err", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		// The link token ties this exchange back to the Telegram user who
		// requested /link; without a live cache entry the POST is a stranger.
		cached := store.LinkTokens.Get(body.LinkToken)
		if cached == nil {
			slog.Info("exchange with unknown or expired link token", "for", ip)
			http.Error(w, "bad token", http.StatusBadRequest)
			return
		}
		store.LinkTokens.Delete(body.LinkToken)
		tgUser := cached.Value()
		slog.Info("begin pub token exchange", "for", ip, "user", tgUser.Username())
		exchangeRequest := plaid.NewItemPublicTokenExchangeRequest(body.PublicToken)
		callCtx, cancel := context.WithTimeout(r.Context(), cfg.PlaidTimeout)
		defer cancel()
		exchangeResponse, _, err := plaidClient.PlaidApi.ItemPublicTokenExchange(callCtx).ItemPublicTokenExchangeRequest(*exchangeRequest).Execute()
		if err != nil {
			slog.Error("unable to generate access token", "for", ip, "err", err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		persistCtx := context.WithoutCancel(r.Context())
		accessToken := exchangeResponse.GetAccessToken()
		slog.Info("successfully linked account", "for", ip)
		slog.Info("fetch associated accounts", "for", ip)
		callCtx, cancel = context.WithTimeout(persistCtx, cfg.PlaidTimeout)
		defer cancel()
		itemRequest := plaid.NewItemGetRequest(accessToken)
		itemResp, _, err := plaidClient.PlaidApi.ItemGet(callCtx).ItemGetRequest(*itemRequest).Execute()
		if err != nil {
			slog.Error("unable to retrieve item", "for", ip, "err", err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		item := itemResp.GetItem()
		slog.Info("retrieved item data", "for", ip)
		atenc, err := EncryptColumnSecret(accessToken, item.ItemId, cfg.DBCryptKey)
		if err != nil {
			slog.Error("unable to encrypt access token", "for", ip, "err", err)
			http.Error(w, "database err", http.StatusInternalServerError)
			return
		}
		err = store.CreateItem(persistCtx, sqlcgen.CreateItemParams{
			ItemID:          item.ItemId,
			UserID:          tgUser.ID(),
			AccessToken:     atenc,
			InstitutionName: StringPtrOk(item.GetInstitutionNameOk()),
			Status:          "active",
		})
		if err != nil {
			slog.Error("unable to insert record", "for", ip, "err", err)
			http.Error(w, "database err", http.StatusInternalServerError)
			return
		}
		slog.Info("successfully added new item to database", "for", ip)
		// fire and forget the heavy stuff; persistCtx outlives the request
		go func() {
			if err := syncItem(persistCtx, item.ItemId, accessToken, nil, plaidClient, store, cfg, notify); err != nil {
				slog.Error("post-link sync failed", "item", item.ItemId, "err", err)
			}
		}()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("linked"))
	}
}
func handleLink(plaidClient *plaid.APIClient, cfg *Config, store *database.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		slog.Info("begin link request", "for", ip)
		token := r.URL.Query().Get("token")
		cached := store.TGTokens.Get(token)
		if cached == nil {
			http.Error(w, "bad token", http.StatusBadRequest)
			return
		}
		// Link tokens are single-use: dead once redeemed. The cached user is
		// the identity here — it was written by an authenticated /link chat.
		store.TGTokens.Delete(token)
		tgUser := cached.Value()
		slog.Info("link token redeemed", "for", ip, "user", tgUser.Username())
		user := plaid.LinkTokenCreateRequestUser{
			ClientUserId: tgUser.ID(),
		}
		depository := plaid.DepositoryFilter{
			AccountSubtypes: []plaid.DepositoryAccountSubtype{
				plaid.DEPOSITORYACCOUNTSUBTYPE_CHECKING,
			},
		}
		credit := plaid.CreditFilter{
			AccountSubtypes: []plaid.CreditAccountSubtype{plaid.CREDITACCOUNTSUBTYPE_CREDIT_CARD},
		}
		accountFilters := plaid.LinkTokenAccountFilters{
			Depository: &depository,
			Credit:     &credit,
		}
		plaidRequest := plaid.NewLinkTokenCreateRequest(
			"Runway",
			"en",
			[]plaid.CountryCode{plaid.COUNTRYCODE_US},
		)
		plaidRequest.SetProducts([]plaid.Products{plaid.PRODUCTS_TRANSACTIONS})
		plaidRequest.SetWebhook(cfg.PlaidWebhookURL)
		plaidRequest.SetAccountFilters(accountFilters)
		plaidRequest.SetUser(user)
		callCtx, cancel := context.WithTimeout(r.Context(), cfg.PlaidTimeout)
		defer cancel()
		linkTokenCreateResp, httpResp, err := plaidClient.PlaidApi.LinkTokenCreate(callCtx).LinkTokenCreateRequest(*plaidRequest).Execute()
		if err != nil {
			var respBody string
			if httpResp != nil {
				b, _ := io.ReadAll(httpResp.Body)
				respBody = string(b)
			}
			slog.Error("unable to generate link token", "for", ip, "err", err, "resp", respBody)
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		plaidLinkToken := linkTokenCreateResp.GetLinkToken()
		store.LinkTokens.Set(plaidLinkToken, tgUser, cfg.TokenTTL)
		templates.LinkPage(plaidLinkToken).Render(r.Context(), w)
	}
}
