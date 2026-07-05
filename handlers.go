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
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}
}
func handleTokenExchange(plaidClient *plaid.APIClient, ctx context.Context, cfg Config, db *sqlcgen.Queries) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		slog.Info("unwrapping pubtoken exchange request", "for", ip)
		var body struct {
			PublicToken string `json:"public_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			slog.Error("parsing body request", "for", ip, "err", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		slog.Info("begin pub token exchange", "for", ip)
		exchangeRequest := plaid.NewItemPublicTokenExchangeRequest(body.PublicToken)
		callCtx, cancel := context.WithTimeout(ctx, cfg.PlaidTimeout)
		defer cancel()
		exchangeResponse, _, err := plaidClient.PlaidApi.ItemPublicTokenExchange(callCtx).ItemPublicTokenExchangeRequest(*exchangeRequest).Execute()
		if err != nil {
			slog.Error("unable to generate access token", "for", ip, "err", err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		accessToken := exchangeResponse.GetAccessToken()
		slog.Info("successfully linked account", "for", ip)
		slog.Info("fetch associated accounts", "for", ip)
		itemCtx, itemCancel := context.WithTimeout(ctx, cfg.PlaidTimeout)
		defer itemCancel()
		itemRequest := plaid.NewItemGetRequest(accessToken)
		itemResp, _, err := plaidClient.PlaidApi.ItemGet(itemCtx).ItemGetRequest(*itemRequest).Execute()
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
			http.Error(w, "database err", 500)
			return
		}
		err = db.CreateItem(ctx, sqlcgen.CreateItemParams{
			ItemID:          item.ItemId,
			AccessToken:     atenc,
			InstitutionName: ToNullString(item.GetInstitutionNameOk()),
			Status:          "active",
		})
		if err != nil {
			slog.Error("unable to insert record", "for", ip, "err", err)
			http.Error(w, "database err", 500)
			return
		}
		slog.Info("successfully added new item to database", "for", ip)
		w.WriteHeader(200)
		w.Write([]byte("linked"))
	}
}
func handleLink(plaidClient *plaid.APIClient, ctx context.Context, cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		slog.Info("begin link request", "for", ip)
		user := plaid.LinkTokenCreateRequestUser{
			ClientUserId: cfg.PlaidClientUserID,
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
		callCtx, cancel := context.WithTimeout(ctx, cfg.PlaidTimeout)
		defer cancel()
		linkTokenCreateResp, httpResp, err := plaidClient.PlaidApi.LinkTokenCreate(callCtx).LinkTokenCreateRequest(*plaidRequest).Execute()
		if err != nil {
			var respBody string
			if httpResp != nil {
				b, _ := io.ReadAll(httpResp.Body)
				respBody = string(b)
			}
			slog.Error("unable to generate link token", "for", ip, "err", err, "resp", respBody)
			http.Error(w, fmt.Sprintf("plaid link token create: %v", err), http.StatusBadGateway)
			return
		}
		plaidLinkToken := linkTokenCreateResp.GetLinkToken()
		templates.LinkPage(plaidLinkToken).Render(r.Context(), w)
	}
}
