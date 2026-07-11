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

// clientIP is for logging only. X-Real-Ip is trusted as set by the reverse
// proxy in front of this service; never use this value for authorization.
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
func handleTokenExchange(plaidClient *plaid.APIClient, store *database.Store, cfg *Config, tg *TelegramBot) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		slog.Info("unwrapping pubtoken exchange request", "for", ip)
		var body struct {
			PublicToken string `json:"public_token"`
			LinkToken   string `json:"link_token"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httpError(r.Context(), w, ip, http.StatusBadRequest, "bad request", "err", err)
			return
		}
		// The link token ties this exchange back to the Telegram user who
		// requested /link; without a live cache entry the POST is a stranger.
		cached := store.LinkTokens.Get(body.LinkToken)
		if cached == nil {
			httpError(r.Context(), w, ip, http.StatusBadRequest, "bad token")
			return
		}
		tgUser := cached.Value()
		slog.Info("begin pub token exchange", "for", ip, "user", tgUser.Username())
		exchangeRequest := plaid.NewItemPublicTokenExchangeRequest(body.PublicToken)
		callCtx, cancel := context.WithTimeout(r.Context(), cfg.PlaidTimeout)
		defer cancel()
		exchangeResponse, _, err := plaidClient.PlaidApi.ItemPublicTokenExchange(callCtx).ItemPublicTokenExchangeRequest(*exchangeRequest).Execute()
		if err != nil {
			httpError(r.Context(), w, ip, http.StatusBadGateway, "bad gateway", "err", err)
			return
		}
		// Consume the token only once the exchange succeeds, so a transient
		// Plaid failure doesn't force the user back to /link.
		store.LinkTokens.Delete(body.LinkToken)
		persistCtx := context.WithoutCancel(r.Context())
		accessToken := exchangeResponse.GetAccessToken()
		slog.Info("successfully linked account", "for", ip)
		slog.Info("fetch associated accounts", "for", ip)
		callCtx, cancel = context.WithTimeout(persistCtx, cfg.PlaidTimeout)
		defer cancel()
		itemRequest := plaid.NewItemGetRequest(accessToken)
		itemResp, _, err := plaidClient.PlaidApi.ItemGet(callCtx).ItemGetRequest(*itemRequest).Execute()
		if err != nil {
			httpError(r.Context(), w, ip, http.StatusBadGateway, "bad gateway", "err", err)
			return
		}
		item := itemResp.GetItem()
		slog.Info("retrieved item data", "for", ip)
		atenc, err := EncryptColumnSecret(accessToken, item.ItemId, cfg.DBCryptKey)
		if err != nil {
			httpError(r.Context(), w, ip, http.StatusInternalServerError, "database err", "err", err)
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
			httpError(r.Context(), w, ip, http.StatusInternalServerError, "database err", "err", err)
			return
		}
		slog.Info("successfully added new item to database", "for", ip)
		firstItem := false
		if n, err := store.CountItemsByUser(persistCtx, tgUser.ID()); err != nil {
			slog.Error("failed to count user items", "for", ip, "err", err)
		} else {
			firstItem = n == 1
		}
		chatID := tgUser.TelegramID()
		institution := stringOr(StringPtrOk(item.GetInstitutionNameOk()), "your bank")
		tg.sendLinkedMessage(persistCtx, chatID, institution, firstItem)
		// fire and forget the heavy stuff; persistCtx outlives the request.
		// The drain worker announces the backfill itself, in order.
		go func() {
			if err := syncItem(persistCtx, item.ItemId, accessToken, nil, plaidClient, store, cfg); err != nil {
				slog.Error("post-link sync failed", "item", item.ItemId, "err", err)
			}
			tg.startDrain(chatID)
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
			httpError(r.Context(), w, ip, http.StatusBadRequest, "bad token")
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
			cfg.PlaidCountryCodeList,
		)
		plaidRequest.SetProducts(cfg.PlaidProductList)
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
			httpError(r.Context(), w, ip, http.StatusBadGateway, "bad gateway", "err", err, "resp", respBody)
			return
		}
		plaidLinkToken := linkTokenCreateResp.GetLinkToken()
		store.LinkTokens.Set(plaidLinkToken, tgUser, cfg.TokenTTL)
		if err := templates.LinkPage(plaidLinkToken, tgUser.FirstName()).Render(r.Context(), w); err != nil {
			slog.Error("failed to render link page", "for", ip, "err", err)
		}
	}
}

func handlePrivacy(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	err := templates.PrivacyPage().Render(r.Context(), w)
	if err != nil {
		httpError(r.Context(), w, ip, http.StatusInternalServerError, "internal error")
	}
}

// httpError logs the error, writes the given status code, and renders the
// styled error page. args are extra slog key/value pairs appended after
// "for", ip.
func httpError(ctx context.Context, w http.ResponseWriter, ip string, code int, msg string, args ...any) {
	slog.Error(msg, append([]any{"for", ip}, args...)...)
	w.WriteHeader(code)
	if err := templates.ErrorPage(code, msg).Render(ctx, w); err != nil {
		slog.Error("failed to render error page", "for", ip, "err", err)
	}
}
