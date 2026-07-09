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
	"strconv"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/hmsmart/runway/database"
	"github.com/hmsmart/runway/database/sqlcgen"
)

type Permission string

const (
	PermissionInvite Permission = "invite"
	PermissionActive Permission = "active"
)

type TelegramBot struct {
	bot    *bot.Bot
	chatID int64
	store  *database.Store
}

func NewTelegramBot(token string, chatID int64, store *database.Store) (*TelegramBot, error) {
	b, err := bot.New(token)
	if err != nil {
		return nil, fmt.Errorf("create telegram bot: %w", err)
	}
	return &TelegramBot{bot: b, chatID: chatID, store: store}, nil
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
		user, err := t.store.GetUserByTelegram(ctx, &chatID)
		if errors.Is(err, sql.ErrNoRows) {
			slog.Info("user not located in database", "chatID", chatID)
		} else if err != nil {
			slog.Error("failed to query database for user", "chatID", chatID, "err", err)
		}
		next(WithUser(ctx, user), b, update)
	}
}

// syncCommands refreshes the per-chat command menu to match the context
// user's state (registered, active, can invite). Callback updates pass
// straight through: they have no message sender and fire on every button tap.
func (t *TelegramBot) syncCommands(next bot.HandlerFunc) bot.HandlerFunc {
	return func(ctx context.Context, b *bot.Bot, update *models.Update) {
		if update.Message != nil {
			user, registered := UserFromContext(ctx)
			if err := t.setCommandMenu(ctx, update.Message.Chat.ID, user, registered); err != nil {
				slog.Error("failed to set bot command menu", "chatID", update.Message.Chat.ID, "err", err)
			}
		}
		next(ctx, b, update)
	}
}

// requirePermission stops the chain unless the context user is active and,
// for permissions beyond PermissionActive, holds that grant too.
func (t *TelegramBot) requirePermission(perm Permission) middleware {
	return func(next bot.HandlerFunc) bot.HandlerFunc {
		return func(ctx context.Context, b *bot.Bot, update *models.Update) {
			user, ok := UserFromContext(ctx)
			allowed := ok && user.Active
			if perm == PermissionInvite {
				allowed = allowed && user.CanInvite
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
		_, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: update.Message.Chat.ID,
			Text:   "You're not authorized. Use /register to get started.",
		})
		if err != nil {
			slog.Error("failed to send denial message", "chatID", update.Message.Chat.ID, "err", err)
		}
	}
}

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
		chain(t.handleLink, t.fetchUser, t.syncCommands, t.requirePermission(PermissionActive)))

	t.bot.RegisterHandler(bot.HandlerTypeCallbackQueryData, "menu:", bot.MatchTypePrefix,
		chain(t.handleMenu, t.fetchUser, t.requirePermission(PermissionActive)))
	t.bot.RegisterHandler(bot.HandlerTypeCallbackQueryData, "exclude:", bot.MatchTypePrefix,
		chain(t.handleExclude("exclude:", 1, "Excluded from spend"), t.fetchUser, t.requirePermission(PermissionActive)))
	t.bot.RegisterHandler(bot.HandlerTypeCallbackQueryData, "include:", bot.MatchTypePrefix,
		chain(t.handleExclude("include:", 0, "Included in spend"), t.fetchUser, t.requirePermission(PermissionActive)))
	t.bot.RegisterHandler(bot.HandlerTypeCallbackQueryData, "amort:", bot.MatchTypePrefix,
		chain(t.handleAmortize, t.fetchUser, t.requirePermission(PermissionActive)))
}

func (t *TelegramBot) handleStart(ctx context.Context, b *bot.Bot, update *models.Update) {
	slog.Info("called start", "chatID", update.Message.Chat.ID)
}

func (t *TelegramBot) handlePing(ctx context.Context, b *bot.Bot, update *models.Update) {
	_, ok := UserFromContext(ctx)
	slog.Info("called ping", "chatID", update.Message.Chat.ID, "registered", ok)
	_, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: update.Message.Chat.ID,
		Text:   "Pong",
	})
	if err != nil {
		slog.Error("failed to send pong", "chatID", update.Message.Chat.ID, "err", err)
	}
}

func (t *TelegramBot) handleLink(ctx context.Context, b *bot.Bot, update *models.Update) {
	slog.Info("got link request", "chatID", update.Message.Chat.ID)
	token := t.store.Tokens.GenerateToken()
	params := url.Values{}
	params.Set("token", token)
	params.Set("tgid", strconv.FormatInt(update.Message.Chat.ID, 10))
	linkURL := "https://gpws.kawaiide.su/link?" + params.Encode()
	_, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: update.Message.Chat.ID,
		Text:   formatLinkMessage(),
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
		t.swapKeyboard(ctx, b, update, amortizeKeyboard(txID))
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

// setCommandMenu writes the command menu a user should see given their state.
// Unregistered and inactive users get register only.
func (t *TelegramBot) setCommandMenu(ctx context.Context, chatID int64, user sqlcgen.User, registered bool) error {
	cmds := []models.BotCommand{
		{Command: "register", Description: "Register with Runway"},
	}
	if registered && user.Active {
		cmds = []models.BotCommand{
			{Command: "ping", Description: "Ping Runway Service"},
			{Command: "link", Description: "Link account"},
		}
		if user.CanInvite {
			cmds = append(cmds, models.BotCommand{Command: "invite", Description: "Invite a user to runway"})
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
	slog.Info("new transaction", "id", tx.TxID, "plaidTx", tx.PlaidTxID, "amt", tx.Amount)
	if tx.Amount < 0 {
		return
	}
	_, err := t.bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      t.chatID,
		Text:        formatTransactionMessage(transactionFromParams(tx)),
		ParseMode:   models.ParseModeHTML,
		ReplyMarkup: transactionKeyboard(tx.TxID, false),
	})
	if err != nil {
		slog.Error("failed to send transaction notification", "tx", tx.TxID, "err", err)
	}
}

func transactionKeyboard(txID string, excluded bool) models.InlineKeyboardMarkup {
	excludeBtn := models.InlineKeyboardButton{Text: "🚫 Exclude", CallbackData: "exclude:" + txID}
	if excluded {
		excludeBtn = models.InlineKeyboardButton{Text: "✅ Include", CallbackData: "include:" + txID}
	}
	return models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{
				{Text: "📊 Amortize", CallbackData: "menu:amort:" + txID},
				excludeBtn,
			},
		},
	}
}

// amortizeKeyboard is the second-level menu shown after tapping Amortize.
func amortizeKeyboard(txID string) models.InlineKeyboardMarkup {
	return models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{
				{Text: "1 Week", CallbackData: "amort:1w:" + txID},
				{Text: "1 Month", CallbackData: "amort:1m:" + txID},
				{Text: "1 Year", CallbackData: "amort:1y:" + txID},
			},
			{
				{Text: "⬅️ Back", CallbackData: "menu:main:" + txID},
			},
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

func formatLinkMessage() string {
	return fmt.Sprintf(
		"🔗 <b>Connect Your Bank Account</b>\n\n" +
			"Tap the link below to securely connect your account through Plaid. " +
			"This link is <b>single-use</b> and expires in 30 minutes.\n\n")
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
