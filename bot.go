package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"math"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/google/uuid"
	"github.com/hmsmart/runway/database"
	"github.com/hmsmart/runway/database/sqlcgen"
	"github.com/hmsmart/runway/domains"
)

// inviteCodePattern matches a normalized invite code: 8 base32 characters,
// uppercased with dashes stripped.
var inviteCodePattern = regexp.MustCompile(`^[A-Z2-7]{8}$`)

type TelegramBot struct {
	bot      *bot.Bot
	baseURL  string
	store    *database.Store
	tokenTTL time.Duration
}

func NewTelegramBot(token string, baseURL string, store *database.Store, tokenTTL time.Duration) (*TelegramBot, error) {
	b, err := bot.New(token)
	if err != nil {
		return nil, fmt.Errorf("create telegram bot: %w", err)
	}
	return &TelegramBot{bot: b, baseURL: baseURL, store: store, tokenTTL: tokenTTL}, nil
}

// middleware wraps a handler with one cross-cutting step.
type middleware func(bot.HandlerFunc) bot.HandlerFunc

// chain wraps h so the middlewares run in the order listed, then h.
func chain(h bot.HandlerFunc, mws ...middleware) bot.HandlerFunc {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// fetchUser resolves the sender's chat to a database user and places it in the
// context for the rest of the chain to read via UserFromContext. Unknown
// senders still flow through, just with no user set.
func (t *TelegramBot) fetchUser(next bot.HandlerFunc) bot.HandlerFunc {
	return func(ctx context.Context, b *bot.Bot, update *models.Update) {
		var chatID int64
		if update.Message != nil {
			chatID = update.Message.Chat.ID
		} else if update.CallbackQuery != nil && update.CallbackQuery.Message.Message != nil {
			chatID = update.CallbackQuery.Message.Message.Chat.ID
		}
		if chatID == 0 {
			// No resolvable chat (e.g. a callback on an inaccessible
			// message); pass through with no user rather than querying tg_id=0.
			next(ctx, b, update)
			return
		}
		row, err := t.store.GetUserByTelegram(ctx, &chatID)
		if errors.Is(err, sql.ErrNoRows) {
			slog.Info("user not located in database", "chatID", chatID)
		} else if err != nil {
			slog.Error("failed to query database for user", "chatID", chatID, "err", err)
		}
		next(WithUser(ctx, domains.NewUser(row)), b, update)
	}
}

// syncCommands refreshes the per-chat command menu to match the context
// user's state (registered, active, can invite). Callback updates pass
// straight through: they have no message sender and fire on every button tap.
func (t *TelegramBot) syncCommands(next bot.HandlerFunc) bot.HandlerFunc {
	return func(ctx context.Context, b *bot.Bot, update *models.Update) {
		if update.Message != nil {
			if err := t.setCommandMenu(ctx, update.Message.Chat.ID, UserFromContext(ctx)); err != nil {
				slog.Error("failed to set bot command menu", "chatID", update.Message.Chat.ID, "err", err)
			}
		}
		next(ctx, b, update)
	}
}

// requirePermission stops the chain unless the context user is active and,
// for permissions beyond PermissionActive, holds that grant too.
// PermissionUnregistered inverts the check: only users with no database row
// pass, so registered users can't re-run registration.
func (t *TelegramBot) requirePermission(perm domains.Permission) middleware {
	return func(next bot.HandlerFunc) bot.HandlerFunc {
		return func(ctx context.Context, b *bot.Bot, update *models.Update) {
			user := UserFromContext(ctx)
			var allowed bool
			if perm == domains.PermissionUnregistered {
				allowed = user == nil
			} else {
				allowed = user.Has(domains.PermissionActive) && user.Has(perm)
			}
			if !allowed {
				slog.Info("update rejected", "perm", perm)
				t.deny(ctx, b, update)
				return
			}
			next(ctx, b, update)
		}
	}
}

func (t *TelegramBot) deny(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.CallbackQuery != nil {
		t.answerCallback(ctx, b, update, "Not authorized")
		return
	}
	if update.Message != nil {
		t.sendText(ctx, b, update.Message.Chat.ID,
			"You're not authorized. If you have an invite code, send /register followed by the code to get started.")
	}
}

// sendText sends a plain-text message and logs delivery failures. Sends that
// need markup or formatting call SendMessage directly.
func (t *TelegramBot) sendText(ctx context.Context, b *bot.Bot, chatID int64, text string) {
	_, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID,
		Text:   text,
	})
	if err != nil {
		slog.Error("failed to send message", "chatID", chatID, "err", err)
	}
}

// errTryLater is the one voice for transient failures the user can't fix.
const errTryLater = "Something went wrong on my end — please try again later."

// amortPeriods maps the callback period token to a SQLite date modifier and a
// human label for the confirmation toast.
var amortPeriods = map[string]struct {
	modifier string
	label    string
}{
	"1w": {"+7 days", "1 week"},
	"1m": {"+1 month", "1 month"},
	"1y": {"+1 year", "1 year"},
}

// RegisterHandlers wires each update to its middleware chain. Every chain
// starts with fetchUser; message commands then sync the command menu, and
// anything that touches data requires an active user.
func (t *TelegramBot) RegisterHandlers() {
	t.bot.RegisterHandler(bot.HandlerTypeMessageText, "/start", bot.MatchTypePrefix,
		chain(t.handleStart, t.fetchUser, t.syncCommands))
	t.bot.RegisterHandler(bot.HandlerTypeMessageText, "/ping", bot.MatchTypeExact,
		chain(t.handlePing, t.fetchUser, t.syncCommands))
	t.bot.RegisterHandler(bot.HandlerTypeMessageText, "/link", bot.MatchTypeExact,
		chain(t.handleLink, t.fetchUser, t.syncCommands, t.requirePermission(domains.PermissionActive)))
	t.bot.RegisterHandler(bot.HandlerTypeMessageText, "/invite", bot.MatchTypeExact,
		chain(t.handleInvite, t.fetchUser, t.syncCommands, t.requirePermission(domains.PermissionInvite)))

	t.bot.RegisterHandler(bot.HandlerTypeMessageText, "/register", bot.MatchTypePrefix,
		chain(t.handleRegistration, t.fetchUser, t.syncCommands, t.requirePermission(domains.PermissionUnregistered)))

	t.bot.RegisterHandler(bot.HandlerTypeCallbackQueryData, "menu:", bot.MatchTypePrefix,
		chain(t.handleMenu, t.fetchUser, t.requirePermission(domains.PermissionActive)))
	t.bot.RegisterHandler(bot.HandlerTypeCallbackQueryData, "exclude:", bot.MatchTypePrefix,
		chain(t.handleExclude("exclude:", 1, "Excluded from spend"), t.fetchUser, t.requirePermission(domains.PermissionActive)))
	t.bot.RegisterHandler(bot.HandlerTypeCallbackQueryData, "include:", bot.MatchTypePrefix,
		chain(t.handleExclude("include:", 0, "Included in spend"), t.fetchUser, t.requirePermission(domains.PermissionActive)))
	t.bot.RegisterHandler(bot.HandlerTypeCallbackQueryData, "amort:", bot.MatchTypePrefix,
		chain(t.handleAmortize, t.fetchUser, t.requirePermission(domains.PermissionActive)))
	t.bot.RegisterHandler(bot.HandlerTypeCallbackQueryData, "mortize:", bot.MatchTypePrefix,
		chain(t.handleMortize, t.fetchUser, t.requirePermission(domains.PermissionActive)))
}

func (t *TelegramBot) handleStart(ctx context.Context, b *bot.Bot, update *models.Update) {
	slog.Info("called start", "chatID", update.Message.Chat.ID)
	if UserFromContext(ctx).Has(domains.PermissionActive) {
		t.sendText(ctx, b, update.Message.Chat.ID,
			"Welcome back to Runway! You're already registered — use /link to connect a bank account.")
		return
	}
	t.sendText(ctx, b, update.Message.Chat.ID,
		"Welcome to Runway! I watch your linked bank accounts and message you when new transactions come in.\n\n"+
			"Runway is invite-only, so you'll need an invite code from an existing user. "+
			"There's no rush — whenever you have one, send /register followed by the code (e.g. /register ABCD-2234).")
}

func (t *TelegramBot) handlePing(ctx context.Context, b *bot.Bot, update *models.Update) {
	user := UserFromContext(ctx)
	slog.Info("called ping", "chatID", update.Message.Chat.ID, "registered", user != nil)
	pong := "Pongthenticated"
	if user == nil {
		pong = "Unpongthenticated"
	}
	t.sendText(ctx, b, update.Message.Chat.ID, pong)
}

func (t *TelegramBot) handleRegistration(ctx context.Context, b *bot.Bot, update *models.Update) {
	parts := strings.Fields(update.Message.Text)
	if len(parts) != 2 {
		slog.Info("invalid arguments passed", "chatID", update.Message.Chat.ID, "message", update.Message.Text)
		t.sendText(ctx, b, update.Message.Chat.ID,
			"To register, send /register followed by your invite code (e.g. /register ABCD-2234 or /register ABCD2234).")
		return
	}
	regcode := strings.ToUpper(parts[1])
	regcode = strings.ReplaceAll(regcode, "-", "")
	if !inviteCodePattern.MatchString(regcode) {
		slog.Info("failed to validate code", "chatID", update.Message.Chat.ID, "code", regcode)
		t.sendText(ctx, b, update.Message.Chat.ID,
			"That invite code doesn't look right. Codes are 8 letters and digits, e.g. ABCD-2234.")
		return
	}
	slog.Info("got valid registration code request, checking db", "chatID", update.Message.Chat.ID, "code", regcode)
	var rows int64
	res, err := t.store.RedeemInviteCode(ctx, sqlcgen.RedeemInviteCodeParams{
		TgID:       &update.Message.Chat.ID,
		InviteCode: regcode,
	})
	if err == nil {
		rows, err = res.RowsAffected()
	}
	if err != nil {
		slog.Error("failed to redeem invite code", "chatID", update.Message.Chat.ID, "code", regcode, "err", err)
		t.sendText(ctx, b, update.Message.Chat.ID, errTryLater)
		return
	}
	if rows != 1 {
		slog.Info("invalid or used code provided", "chatID", update.Message.Chat.ID, "code", regcode)
		t.sendText(ctx, b, update.Message.Chat.ID,
			"That invite code doesn't exist or has already been redeemed.")
		return
	}
	// chain only builds a handler; call it to run. fetchUser reloads the
	// now-registered user so syncCommands writes the active command menu
	// before the welcome message goes out.
	welcome := func(ctx context.Context, b *bot.Bot, update *models.Update) {
		slog.Info("registration successful", "chatID", update.Message.Chat.ID, "code", regcode)
		t.sendText(ctx, b, update.Message.Chat.ID,
			"Welcome to Runway! You're registered — use /link to connect your first bank account.")
	}
	chain(welcome, t.fetchUser, t.syncCommands)(ctx, b, update)
}

func (t *TelegramBot) handleInvite(ctx context.Context, b *bot.Bot, update *models.Update) {
	invUser := UserFromContext(ctx)
	inviteCode := RandomToken(5, Base32)
	userID, err := uuid.NewV7()
	if err != nil {
		slog.Error("failed to generate uuid", "chatID", invUser.TelegramID(), "err", err)
		t.sendText(ctx, b, update.Message.Chat.ID, errTryLater)
		return
	}
	err = t.store.CreateInviteCode(ctx, sqlcgen.CreateInviteCodeParams{
		ID:         userID.String(),
		InviteCode: inviteCode,
	})
	if err != nil {
		slog.Error("failed to insert invite", "chatID", invUser.TelegramID(), "err", err)
		t.sendText(ctx, b, update.Message.Chat.ID, errTryLater)
		return
	}
	slog.Info("invite created", "chatID", invUser.TelegramID())
	// Hyphenate for readability; /register strips the dash before matching.
	displayCode := inviteCode[:4] + "-" + inviteCode[4:]
	t.sendText(ctx, b, update.Message.Chat.ID,
		fmt.Sprintf("Invite created! Share this invite code with the invitee: %s\nThey can redeem it by sending me /register %s", displayCode, displayCode))
}
func (t *TelegramBot) handleLink(ctx context.Context, b *bot.Bot, update *models.Update) {
	slog.Info("got link request", "chatID", update.Message.Chat.ID)
	user := UserFromContext(ctx)
	if user == nil {
		slog.Error("something happened where a user was allowed to /link without proper context")
		t.sendText(ctx, b, update.Message.Chat.ID, errTryLater)
		return
	}
	token := RandomToken(16, Base64)
	t.store.TGTokens.Set(token, *user, t.tokenTTL)
	params := url.Values{}
	params.Set("token", token)
	linkURL := t.baseURL + "/link?" + params.Encode()
	_, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: update.Message.Chat.ID,
		Text:   formatLinkMessage(t.tokenTTL),
		ReplyMarkup: &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{{
				{Text: "🔗 Link Account", URL: linkURL},
			}},
		},
		ParseMode: "HTML",
	})
	if err != nil {
		slog.Error("failed to send link message", "chatID", update.Message.Chat.ID, "err", err)
	}
}

func (t *TelegramBot) handleMenu(ctx context.Context, b *bot.Bot, update *models.Update) {
	parts := strings.SplitN(update.CallbackQuery.Data, ":", 3)
	if len(parts) != 3 {
		t.answerCallback(ctx, b, update, "Something went wrong")
		return
	}
	menu, txID := parts[1], parts[2]
	switch menu {
	case "amort":
		// The amortize menu only offers Mortize when there's an end date
		// to clear, so read the row's current state.
		tx, err := t.store.GetTransaction(ctx, txID)
		if err != nil {
			slog.Error("failed to load transaction for amortize menu", "tx", txID, "err", err)
			t.answerCallback(ctx, b, update, "Something went wrong")
			return
		}
		t.swapKeyboard(ctx, b, update, amortizeKeyboard(txID, tx.AmortEnd != nil))
	case "main":
		// The main keyboard depends on the row's excluded state, so
		// rebuild the whole message from the database.
		t.refreshMessage(ctx, b, update, txID)
	default:
		t.answerCallback(ctx, b, update, "Something went wrong")
		return
	}
	t.answerCallback(ctx, b, update, "")
}

// handleExclude builds the exclude:/include: handler; the two differ only in
// the flag written and the toast. The refreshed keyboard offers whichever
// action applies to the row's new state.
func (t *TelegramBot) handleExclude(prefix string, excluded int64, toast string) bot.HandlerFunc {
	return func(ctx context.Context, b *bot.Bot, update *models.Update) {
		txID := strings.TrimPrefix(update.CallbackQuery.Data, prefix)
		err := t.store.SetExcluded(ctx, sqlcgen.SetExcludedParams{
			Excluded: excluded,
			TxID:     txID,
		})
		if err != nil {
			slog.Error("failed to set transaction exclusion", "tx", txID, "excluded", excluded, "err", err)
			t.answerCallback(ctx, b, update, "Something went wrong")
			return
		}
		t.refreshMessage(ctx, b, update, txID)
		t.answerCallback(ctx, b, update, toast)
	}
}

func (t *TelegramBot) handleAmortize(ctx context.Context, b *bot.Bot, update *models.Update) {
	parts := strings.SplitN(update.CallbackQuery.Data, ":", 3)
	if len(parts) != 3 {
		t.answerCallback(ctx, b, update, "Something went wrong")
		return
	}
	period, ok := amortPeriods[parts[1]]
	txID := parts[2]
	if !ok {
		t.answerCallback(ctx, b, update, "Something went wrong")
		return
	}
	err := t.store.SetAmortEnd(ctx, sqlcgen.SetAmortEndParams{
		Modifier: period.modifier,
		TxID:     txID,
	})
	if err != nil {
		slog.Error("failed to amortize transaction", "tx", txID, "err", err)
		t.answerCallback(ctx, b, update, "Something went wrong")
		return
	}
	t.refreshMessage(ctx, b, update, txID)
	t.answerCallback(ctx, b, update, "Amortizing over "+period.label)
}

// handleMortize clears a transaction's amortization end date.
func (t *TelegramBot) handleMortize(ctx context.Context, b *bot.Bot, update *models.Update) {
	txID := strings.TrimPrefix(update.CallbackQuery.Data, "mortize:")
	if err := t.store.ClearAmortEnd(ctx, txID); err != nil {
		slog.Error("failed to clear transaction amortization", "tx", txID, "err", err)
		t.answerCallback(ctx, b, update, "Something went wrong")
		return
	}
	t.refreshMessage(ctx, b, update, txID)
	t.answerCallback(ctx, b, update, "Amortization removed")
}

// setCommandMenu writes the command menu a user should see given their
// permissions. Unregistered (nil) and inactive users get ping and register.
func (t *TelegramBot) setCommandMenu(ctx context.Context, chatID int64, user *domains.User) error {
	cmds := []models.BotCommand{
		{Command: "ping", Description: "Check that Runway is up"},
		{Command: "register", Description: "Register with an invite code"},
	}
	if user.Has(domains.PermissionActive) {
		cmds = []models.BotCommand{
			{Command: "ping", Description: "Check that Runway is up"},
			{Command: "link", Description: "Link a bank account"},
		}
		if user.Has(domains.PermissionInvite) {
			cmds = append(cmds, models.BotCommand{Command: "invite", Description: "Create an invite code for a new user"})
		}
	}
	// BotCommandScopeChatMember only applies to group chats; in a private
	// chat the chat is the user, so chat scope gives a per-user menu.
	_, err := t.bot.SetMyCommands(ctx, &bot.SetMyCommandsParams{
		Commands: cmds,
		Scope: &models.BotCommandScopeChat{
			ChatID: chatID,
		},
	})
	return err
}

// swapKeyboard replaces the inline keyboard on the message a callback came
// from, e.g. expanding Amortize into its period options.
func (t *TelegramBot) swapKeyboard(ctx context.Context, b *bot.Bot, update *models.Update, kb models.InlineKeyboardMarkup) {
	msg := update.CallbackQuery.Message.Message
	if msg == nil {
		return
	}
	_, err := b.EditMessageReplyMarkup(ctx, &bot.EditMessageReplyMarkupParams{
		ChatID:      msg.Chat.ID,
		MessageID:   msg.ID,
		ReplyMarkup: kb,
	})
	if err != nil {
		slog.Error("failed to edit message keyboard", "err", err)
	}
}

// refreshMessage rewrites a notification's body from the row's current state
// (amortized/excluded status lines) and resets the keyboard to the main row.
func (t *TelegramBot) refreshMessage(ctx context.Context, b *bot.Bot, update *models.Update, txID string) {
	msg := update.CallbackQuery.Message.Message
	if msg == nil {
		return
	}
	tx, err := t.store.GetTransaction(ctx, txID)
	if err != nil {
		slog.Error("failed to load transaction for message refresh", "tx", txID, "err", err)
		return
	}
	_, err = b.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID:      msg.Chat.ID,
		MessageID:   msg.ID,
		Text:        formatTransactionMessage(tx),
		ParseMode:   models.ParseModeHTML,
		ReplyMarkup: transactionKeyboard(txID, tx.Excluded == 1),
	})
	if err != nil {
		slog.Error("failed to refresh transaction message", "tx", txID, "err", err)
	}
}

func (t *TelegramBot) answerCallback(ctx context.Context, b *bot.Bot, update *models.Update, text string) {
	_, err := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
		CallbackQueryID: update.CallbackQuery.ID,
		Text:            text,
	})
	if err != nil {
		slog.Error("failed to answer callback query", "err", err)
	}
}

// NotifyTransaction announces a newly synced transaction to the configured
// chat. Positive amounts are money out (Plaid's convention); credits are
// skipped.
func (t *TelegramBot) NotifyTransaction(ctx context.Context, tx sqlcgen.UpsertTransactionParams) {
	user := UserFromContext(ctx)
	if user == nil {
		slog.Info("transaction not associated with user", "id", tx.TxID, "plaidTx", tx.PlaidTxID, "amt", tx.Amount)
		return
	}
	if tx.Amount < 0 {
		return
	}
	slog.Info("new transaction", "chatID", user.TelegramID(), "id", tx.TxID, "plaidTx", tx.PlaidTxID, "amt", tx.Amount)
	_, err := t.bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      user.TelegramID(),
		Text:        formatTransactionMessage(transactionFromParams(tx)),
		ParseMode:   models.ParseModeHTML,
		ReplyMarkup: transactionKeyboard(tx.TxID, false),
	})
	if err != nil {
		slog.Error("failed to send transaction notification", "chatID", user.TelegramID(), "tx", tx.TxID, "err", err)
	}
}

func transactionKeyboard(txID string, excluded bool) models.InlineKeyboardMarkup {
	if excluded {
		return models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{
				{
					{Text: "✅ Include", CallbackData: "include:" + txID},
				},
			},
		}
	}
	return models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{
				{Text: "📊 Amortize", CallbackData: "menu:amort:" + txID},
				{Text: "🚫 Exclude", CallbackData: "exclude:" + txID},
			},
		},
	}
}

// amortizeKeyboard is the second-level menu shown after tapping Amortize.
// The period row always shows so a mis-tapped period can be corrected by
// tapping again; Mortize (clear) only appears once the row is amortized.
func amortizeKeyboard(txID string, amortized bool) models.InlineKeyboardMarkup {
	bottom := []models.InlineKeyboardButton{
		{Text: "⬅️ Back", CallbackData: "menu:main:" + txID},
	}
	if amortized {
		bottom = append([]models.InlineKeyboardButton{
			{Text: "❌ Mortize", CallbackData: "mortize:" + txID},
		}, bottom...)
	}
	return models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{
				{Text: "1 Week", CallbackData: "amort:1w:" + txID},
				{Text: "1 Month", CallbackData: "amort:1m:" + txID},
				{Text: "1 Year", CallbackData: "amort:1y:" + txID},
			},
			bottom,
		},
	}
}

// transactionFromParams adapts freshly upserted params to the row model so
// notifications and post-action refreshes share one message formatter. New
// rows have no amortization/exclusion state yet, so those default to unset.
func transactionFromParams(p sqlcgen.UpsertTransactionParams) sqlcgen.Transaction {
	return sqlcgen.Transaction{
		TxID:             p.TxID,
		PlaidTxID:        p.PlaidTxID,
		AccountID:        p.AccountID,
		Date:             p.Date,
		Amount:           p.Amount,
		Name:             p.Name,
		MerchantName:     p.MerchantName,
		CategoryPrimary:  p.CategoryPrimary,
		CategoryDetailed: p.CategoryDetailed,
		PaymentChannel:   p.PaymentChannel,
		Pending:          p.Pending,
		RawJson:          p.RawJson,
	}
}

func formatLinkMessage(ttl time.Duration) string {
	return fmt.Sprintf(
		"🔗 <b>Connect Your Bank Account</b>\n\n"+
			"Tap the link below to securely connect your account through Plaid. "+
			"This link is <b>single-use</b> and expires in %s.\n\n", humanDuration(ttl))
}

// humanDuration renders a duration for user-facing text, e.g. "30 minutes".
func humanDuration(d time.Duration) string {
	if d >= time.Hour && d%time.Hour == 0 {
		if h := int(d / time.Hour); h != 1 {
			return fmt.Sprintf("%d hours", h)
		}
		return "1 hour"
	}
	if m := int(d / time.Minute); m != 1 {
		return fmt.Sprintf("%d minutes", m)
	}
	return "1 minute"
}

func formatTransactionMessage(tx sqlcgen.Transaction) string {
	var b strings.Builder

	// Header — merchant or name, fallback to "Unknown"
	label := stringOr(tx.MerchantName, tx.Name)
	if label == "" {
		label = "Unknown"
	}

	// Positive amount = money out (debit), negative = money in (credit)
	var emoji, sign string
	if tx.Amount >= 0 {
		emoji = "💸"
		sign = "-"
	} else {
		emoji = "💰"
		sign = "+"
	}
	absAmount := math.Abs(tx.Amount)

	b.WriteString(fmt.Sprintf("%s <b>%s$%.2f</b>  %s\n", emoji, sign, absAmount, html.EscapeString(label)))

	if tx.CategoryPrimary != "" {
		cat := displayCategory(tx.CategoryPrimary)
		if tx.CategoryDetailed != "" {
			cat += " › " + displayCategory(tx.CategoryDetailed)
		}
		b.WriteString(fmt.Sprintf("🏷 %s\n", html.EscapeString(cat)))
	}

	b.WriteString(fmt.Sprintf("📅 %s", tx.Date))

	if tx.PaymentChannel != "" {
		b.WriteString(fmt.Sprintf("  ·  %s", html.EscapeString(tx.PaymentChannel)))
	}

	if tx.Pending == 1 {
		b.WriteString("\n⏳ <i>pending</i>")
	}

	if tx.AmortEnd != nil {
		b.WriteString(fmt.Sprintf("\n📊 <i>amortized until %s</i>", html.EscapeString(*tx.AmortEnd)))
	}

	if tx.Excluded == 1 {
		b.WriteString("\n🚫 <i>excluded from spend</i>")
	}

	return b.String()
}
