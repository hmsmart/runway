package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/hmsmart/runway/database"
	"github.com/hmsmart/runway/database/sqlcgen"
	"github.com/hmsmart/runway/domains"
	"github.com/hmsmart/runway/templates"
	"github.com/plaid/plaid-go/v43/plaid"
)

// clientIP is for logging only. X-Real-Ip is trusted as set by the reverse
// proxy in front of this service; never use this value for authorization.
func clientIP(r *http.Request) string {
	cfConIP := r.Header.Get("CF-Connecting-IP")
	if cfConIP != "" {
		return cfConIP
	}
	if xri := r.Header.Get("X-Real-Ip"); xri != "" {
		return xri
	}
	ips := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	clientIP := strings.TrimSpace(ips[0])
	return clientIP
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
		// Log the item id as soon as the exchange yields it: if anything past
		// this point fails, the item exists at Plaid but not in our database,
		// and this id is what support/repair needs.
		slog.Info("successfully linked account", "for", ip, "item", exchangeResponse.GetItemId())
		slog.Info("fetch associated accounts", "for", ip, "item", exchangeResponse.GetItemId())
		callCtx, cancel = context.WithTimeout(persistCtx, cfg.PlaidTimeout)
		defer cancel()
		itemRequest := plaid.NewItemGetRequest(accessToken)
		itemResp, _, err := plaidClient.PlaidApi.ItemGet(callCtx).ItemGetRequest(*itemRequest).Execute()
		if err != nil {
			httpError(r.Context(), w, ip, http.StatusBadGateway, "bad gateway", "item", exchangeResponse.GetItemId(), "err", err)
			return
		}
		item := itemResp.GetItem()
		slog.Info("retrieved item data", "for", ip, "item", item.ItemId)
		atenc, err := EncryptColumnSecret(accessToken, item.ItemId, cfg.DBCryptKey)
		if err != nil {
			httpError(r.Context(), w, ip, http.StatusInternalServerError, "database err", "item", item.ItemId, "err", err)
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
			httpError(r.Context(), w, ip, http.StatusInternalServerError, "database err", "item", item.ItemId, "err", err)
			return
		}
		slog.Info("successfully added new item to database", "for", ip, "item", item.ItemId)
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

// consumeMagicToken redeems a single-use Telegram magic-link token and logs
// the browser in: the cached user becomes a 24h session. Returns nil if the
// token is unknown or already spent.
func consumeMagicToken(w http.ResponseWriter, store *database.Store, token string) *domains.User {
	cached := store.TGTokens.Get(token)
	if cached == nil {
		return nil
	}
	// Magic tokens are single-use: dead once redeemed. The cached user is
	// the identity here — it was written by an authenticated Telegram chat.
	store.TGTokens.Delete(token)
	user := cached.Value()
	createSession(w, store, user)
	return &user
}

// handleMagicConfirm is the POST target of the confirm page: it burns the
// magic token, starts the session, and bounces to dest. Consumption lives
// behind a POST so link previewers and in-app browsers that merely GET the
// magic URL can't spend the token before the user's real browser does. A
// replayed POST from a browser that already holds a session passes through.
func handleMagicConfirm(store *database.Store, dest string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if user := consumeMagicToken(w, store, r.FormValue("token")); user != nil {
			slog.Info("magic token confirmed", "for", ip, "user", user.Username(), "dest", dest)
		} else if domains.UserFromContext(r.Context()) == nil {
			// No live token and no prior session: nothing vouches for this
			// browser.
			httpError(r.Context(), w, ip, http.StatusBadRequest, "bad token")
			return
		}
		http.Redirect(w, r, dest, http.StatusSeeOther)
	}
}

func handleLink(plaidClient *plaid.APIClient, cfg *Config, store *database.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		slog.Info("begin link request", "for", ip)
		// A magic link lands here with a live token: show the confirm step
		// rather than consuming anything, so a preview fetch can't burn the
		// token (or a Plaid call). The confirm POST creates the session and
		// redirects back here, joining the nav's session-only path below.
		if token := r.URL.Query().Get("token"); store.TGTokens.Has(token) {
			if err := templates.ConfirmPage("/link", token).Render(r.Context(), w); err != nil {
				slog.Error("failed to render confirm page", "for", ip, "err", err)
			}
			return
		}
		sessionUser := domains.UserFromContext(r.Context())
		if sessionUser == nil {
			httpError(r.Context(), w, ip, http.StatusUnauthorized, "not authorized — ask the bot for a fresh link")
			return
		}
		tgUser := *sessionUser
		slog.Info("link request authenticated", "for", ip, "user", tgUser.Username())
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
		// Plaid only pulls 90 days of history by default; ask for more so the
		// initial sync backfills real history.
		transactions := plaid.NewLinkTokenTransactions()
		transactions.SetDaysRequested(cfg.PlaidHistoryDays)
		plaidRequest.SetTransactions(*transactions)
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

// handleDash is where a /dash magic link lands: a live token gets the
// confirm step (the POST does the consuming), an existing session skips
// straight to the dashboard, and anything else is a dead link.
func handleDash(store *database.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if token := r.URL.Query().Get("token"); store.TGTokens.Has(token) {
			if err := templates.ConfirmPage("/dash", token).Render(r.Context(), w); err != nil {
				slog.Error("failed to render confirm page", "for", ip, "err", err)
			}
			return
		}
		if domains.UserFromContext(r.Context()) != nil {
			http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
			return
		}
		httpError(r.Context(), w, ip, http.StatusBadRequest, "bad token")
	}
}

func handleDashboard(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	// requireSession guarantees a user is present.
	user := domains.UserFromContext(r.Context())
	if err := templates.DashboardPage(user.FirstName()).Render(r.Context(), w); err != nil {
		httpError(r.Context(), w, ip, http.StatusInternalServerError, "internal error")
	}
}

func handleTransactions(store *database.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		// requireSession guarantees a user is present.
		user := domains.UserFromContext(r.Context())
		rows, err := store.ListTransactionsByUser(r.Context(), user.ID())
		if err != nil {
			httpError(r.Context(), w, ip, http.StatusInternalServerError, "internal error", "err", err)
			return
		}
		transList := make([]domains.TransactionRow, 0, len(rows))
		for _, row := range rows {
			transList = append(transList, domains.NewTransactionRow(
				row.Date, row.AccountName, row.Description, row.Amount,
				row.Excluded != 0, row.RawDate, row.AmortEnd, row.LogoUrl,
			))
		}
		if err := templates.TransactionPage(user.FirstName(), transList).Render(r.Context(), w); err != nil {
			httpError(r.Context(), w, ip, http.StatusInternalServerError, "internal error")
		}
	}
}

// handleLogin points browsers without a session at the bot; anyone already
// signed in skips straight to the dashboard.
func handleLogin(w http.ResponseWriter, r *http.Request) {
	if domains.UserFromContext(r.Context()) != nil {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	if err := templates.LoginPage().Render(r.Context(), w); err != nil {
		httpError(r.Context(), w, clientIP(r), http.StatusInternalServerError, "internal error")
	}
}

func handlePrivacy(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	err := templates.PrivacyPage().Render(r.Context(), w)
	if err != nil {
		httpError(r.Context(), w, ip, http.StatusInternalServerError, "internal error")
	}
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	err := templates.IndexPage().Render(r.Context(), w)
	if err != nil {
		httpError(r.Context(), w, ip, http.StatusInternalServerError, "internal error")
	}
}

// handleProfilePic serves the viewer's own avatar: the session user's cached
// photo, or the default for anonymous visitors (and photo-cache misses). The
// session is the identity, so the URL carries no user ID at all.
func handleProfilePic(store *database.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		photo := domains.GetDefaultPhoto()
		if user := domains.UserFromContext(r.Context()); user != nil {
			if cached := store.TGPhotos.Get(user.ID()); cached != nil {
				p := cached.Value()
				photo = &p
			}
		}
		// One URL answers differently per session, so shared caches must not
		// store it and the browser revalidates on login/logout; the ETag
		// (Telegram's stable per-file ID) makes revalidation a cheap 304.
		etag := `"` + photo.ID() + `"`
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "private, no-cache")
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", photo.MIME())
		w.Write(photo.Data())
	}
}

func handleStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if strings.HasSuffix(r.URL.Path, "/") {
			httpError(r.Context(), w, ip, http.StatusNotFound, "page not found", "path", r.URL.RequestURI())
			return
		}
		// Embedded assets carry no modtime, so downstream caches (Cloudflare)
		// would otherwise apply long defaults and serve stale files after a
		// redeploy. no-cache forces revalidation against the origin.
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

func handleError(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	err := templates.ErrorPage(http.StatusNotFound, "page not found").Render(r.Context(), w)
	if err != nil {
		httpError(r.Context(), w, ip, http.StatusNotFound, "page not found", "path", r.URL.RequestURI())
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
