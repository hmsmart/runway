package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/hmsmart/runway/database/sqlcgen"
	"github.com/hmsmart/runway/templates"
	"github.com/plaid/plaid-go/v43/plaid"
)

func clientIP(r *http.Request) string {
	// X-Forwarded-For can be a chain: client, proxy1, proxy2
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// first entry is the original client
		ip, _, _ := strings.Cut(xff, ",")
		return strings.TrimSpace(ip)
	}
	if xri := r.Header.Get("X-Real-Ip"); xri != "" {
		return xri
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func handleHealthz(dbConn *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := dbConn.PingContext(r.Context()); err != nil {

			http.Error(w, "db unreachable", http.StatusServiceUnavailable)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}
}
func handleTokenExchange(plaidClient *plaid.APIClient, ctx context.Context, cfg Config, db *sqlcgen.Queries) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slog.Info("unwrapping pubtoken exchange request", "for", clientIP(r))
		var body struct {
			PublicToken string `json:"public_token"`
			Accounts    []struct {
				ID      string `json:"id"`
				Name    string `json:"name"`
				Mask    string `json:"mask"`
				Type    string `json:"type"`
				Subtype string `json:"subtype"`
			} `json:"accounts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			slog.Error("parsing body request", "for", clientIP(r), "err", err)
			http.Error(w, "bad request", http.StatusBadRequest)
		}
		slog.Info("begin pub token exchange", "for", clientIP(r), "pubtoken", body.PublicToken)
		exchangeRequest := plaid.NewItemPublicTokenExchangeRequest(body.PublicToken)
		callCtx, cancel := context.WithTimeout(ctx, cfg.PlaidTimeout)
		defer cancel()
		exchangeResponse, _, err := plaidClient.PlaidApi.ItemPublicTokenExchange(callCtx).ItemPublicTokenExchangeRequest(*exchangeRequest).Execute()
		if err != nil {
			slog.Error("unable to generate access token", "for", clientIP(r), "err", err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
		}
		accessToken := exchangeResponse.GetAccessToken()
		slog.Info("successfully linked account", "for", clientIP(r), "tok", accessToken)
		slog.Info("fetch associated accounts", "for", clientIP(r))
		callCtx, cancel = context.WithTimeout(ctx, cfg.PlaidTimeout)
		defer cancel()
		itemRequest := plaid.NewItemGetRequest(accessToken)
		itemResp, _, err := plaidClient.PlaidApi.ItemGet(ctx).ItemGetRequest(*itemRequest).Execute()
		if err != nil {
			slog.Error("unable to retrieve item", "for", clientIP(r), "err", err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
		}
		item := itemResp.GetItem()
		slog.Info("retrieved item data", "for", clientIP(r))
		atenc, err := EncryptColumnSecret(accessToken, item.ItemId, cfg.DBCryptKey)
		if err != nil {
			slog.Error("unable to encrypt access token", "for", clientIP(r), "err", err)
			http.Error(w, "database err", 500)
		}
		err = db.CreateItem(ctx, sqlcgen.CreateItemParams{
			ItemID:          item.ItemId,
			AccessToken:     atenc,
			InstitutionName: ToNullString(item.GetInstitutionNameOk()),
			Status:          "active",
		})
		if err != nil {
			slog.Error("unable to insert record", "for", clientIP(r), "err", err)
			http.Error(w, "database err", 500)
		}
		slog.Info("successfully added new item to database", "for", clientIP(r))
		w.WriteHeader(200)
		w.Write([]byte("linked"))
	}
}
func handleLink(plaidClient *plaid.APIClient, ctx context.Context, cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slog.Info("begin link request", "for", clientIP(r))
		user := plaid.LinkTokenCreateRequestUser{
			ClientUserId: "hayden",
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
		plaidRequest.SetWebhook("https://gpws.kawaiide.su/hook/plaid")
		plaidRequest.SetAccountFilters(accountFilters)
		plaidRequest.SetUser(user)
		callCtx, cancel := context.WithTimeout(ctx, cfg.PlaidTimeout)
		defer cancel()
		linkTokenCreateResp, httpResp, err := plaidClient.PlaidApi.LinkTokenCreate(callCtx).LinkTokenCreateRequest(*plaidRequest).Execute()
		if err != nil {
			slog.Error("unable to generate link token", "for", clientIP(r), "err", err)
			body, _ := io.ReadAll(httpResp.Body)
			fmt.Printf("%v", string(body))
			http.Error(w, fmt.Sprintf("plaid link token create: %v", err), http.StatusBadGateway)
			return
		}
		plaidLinkToken := linkTokenCreateResp.GetLinkToken()
		templates.LinkPage(plaidLinkToken).Render(r.Context(), w)
	}
}

func amortizeTxn(dbConn *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

	}
}
