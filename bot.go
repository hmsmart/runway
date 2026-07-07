package main

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"math"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/hmsmart/runway/database"
	"github.com/hmsmart/runway/database/sqlcgen"
)

type TelegramBot struct {
	bot    *bot.Bot
	chatID int64
}

func NewTelegramBot(token string, chatID int64) (*TelegramBot, error) {
	b, err := bot.New(token)
	if err != nil {
		return nil, fmt.Errorf("create telegram bot: %w", err)
	}
	return &TelegramBot{bot: b, chatID: chatID}, nil
}

func (t *TelegramBot) userFilter(next bot.HandlerFunc) bot.HandlerFunc {
	return func(ctx context.Context, b *bot.Bot, update *models.Update) {
		chatID := int64(0)
		if update.Message != nil {
			chatID = update.Message.Chat.ID
		} else if update.CallbackQuery != nil && update.CallbackQuery.Message.Message != nil {
			chatID = update.CallbackQuery.Message.Message.Chat.ID
		}
		if chatID != t.chatID {
			return
		}
		next(ctx, b, update)
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

func (t *TelegramBot) RegisterHandlers(store *database.Store) {
	t.bot.RegisterHandler(bot.HandlerTypeMessageText, "/ping", bot.MatchTypeExact,
		t.userFilter(func(ctx context.Context, b *bot.Bot, update *models.Update) {
			slog.Info("got ping", "chatID", update.Message.Chat.ID)
			b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: update.Message.Chat.ID,
				Text:   "pong",
			})
		}),
	)
	t.bot.RegisterHandler(bot.HandlerTypeCallbackQueryData, "menu:", bot.MatchTypePrefix,
		t.userFilter(func(ctx context.Context, b *bot.Bot, update *models.Update) {
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
				t.refreshMessage(ctx, b, update, store, txID)
			default:
				t.answerCallback(ctx, b, update, "Something went wrong")
				return
			}
			t.answerCallback(ctx, b, update, "")
		}),
	)
	// exclude:/include: toggle the excluded flag; the refreshed keyboard
	// offers whichever action applies to the row's new state.
	excludeHandler := func(prefix string, excluded int64, toast string) bot.HandlerFunc {
		return func(ctx context.Context, b *bot.Bot, update *models.Update) {
			txID := strings.TrimPrefix(update.CallbackQuery.Data, prefix)
			err := store.SetExcluded(ctx, sqlcgen.SetExcludedParams{
				Excluded: excluded,
				TxID:     txID,
			})
			if err != nil {
				slog.Error("failed to set transaction exclusion", "tx", txID, "excluded", excluded, "err", err)
				t.answerCallback(ctx, b, update, "Something went wrong")
				return
			}
			t.refreshMessage(ctx, b, update, store, txID)
			t.answerCallback(ctx, b, update, toast)
		}
	}
	t.bot.RegisterHandler(bot.HandlerTypeCallbackQueryData, "exclude:", bot.MatchTypePrefix,
		t.userFilter(excludeHandler("exclude:", 1, "Excluded from spend")))
	t.bot.RegisterHandler(bot.HandlerTypeCallbackQueryData, "include:", bot.MatchTypePrefix,
		t.userFilter(excludeHandler("include:", 0, "Included in spend")))
	t.bot.RegisterHandler(bot.HandlerTypeCallbackQueryData, "amort:", bot.MatchTypePrefix,
		t.userFilter(func(ctx context.Context, b *bot.Bot, update *models.Update) {
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
			err := store.SetAmortEnd(ctx, sqlcgen.SetAmortEndParams{
				Modifier: period.modifier,
				TxID:     txID,
			})
			if err != nil {
				slog.Error("failed to amortize transaction", "tx", txID, "err", err)
				t.answerCallback(ctx, b, update, "Something went wrong")
				return
			}
			t.refreshMessage(ctx, b, update, store, txID)
			t.answerCallback(ctx, b, update, "Amortizing over "+period.label)
		}),
	)
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
func (t *TelegramBot) refreshMessage(ctx context.Context, b *bot.Bot, update *models.Update, store *database.Store, txID string) {
	msg := update.CallbackQuery.Message.Message
	if msg == nil {
		return
	}
	tx, err := store.GetTransaction(ctx, txID)
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

func formatTransactionMessage(tx sqlcgen.Transaction) string {
	var b strings.Builder

	// Header — merchant or name, fallback to "Unknown"
	label := stringOr(tx.MerchantName, stringOr(tx.Name, "Unknown"))

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

	if tx.CategoryPrimary.Valid {
		cat := displayCategory(tx.CategoryPrimary.String)
		if tx.CategoryDetailed.Valid {
			cat += " › " + displayCategory(tx.CategoryDetailed.String)
		}
		b.WriteString(fmt.Sprintf("🏷 %s\n", html.EscapeString(cat)))
	}

	b.WriteString(fmt.Sprintf("📅 %s", tx.Date))

	if tx.PaymentChannel.Valid {
		b.WriteString(fmt.Sprintf("  ·  %s", html.EscapeString(tx.PaymentChannel.String)))
	}

	if tx.Pending == 1 {
		b.WriteString("\n⏳ <i>pending</i>")
	}

	if tx.AmortEnd.Valid {
		b.WriteString(fmt.Sprintf("\n📊 <i>amortized until %s</i>", html.EscapeString(tx.AmortEnd.String)))
	}

	if tx.Excluded == 1 {
		b.WriteString("\n🚫 <i>excluded from spend</i>")
	}

	return b.String()
}
