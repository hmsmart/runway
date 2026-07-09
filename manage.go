package main

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"math"
	"strconv"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/hmsmart/runway/database/sqlcgen"
	"github.com/plaid/plaid-go/v43/plaid"
)

// handleLinks lists the user's linked institutions, numbered for /unlink.
func (t *TelegramBot) handleLinks(ctx context.Context, b *bot.Bot, update *models.Update) {
	user := UserFromContext(ctx)
	chatID := update.Message.Chat.ID
	items, err := t.store.ListItemsByUser(ctx, user.ID())
	if err != nil {
		slog.Error("failed to list items", "chatID", chatID, "err", err)
		t.sendText(ctx, b, chatID, errTryLater)
		return
	}
	if len(items) == 0 {
		t.sendText(ctx, b, chatID, "You have no linked accounts. Use /link to connect one.")
		return
	}
	var sb strings.Builder
	sb.WriteString("🏦 <b>Your linked accounts</b>\n")
	for i, item := range items {
		sb.WriteString(fmt.Sprintf("\n<b>%d.</b> %s — linked %s\n",
			i+1, html.EscapeString(stringOr(item.InstitutionName, "Unknown institution")),
			item.CreatedAt.Format("Jan 2, 2006")))
		accounts, err := t.store.ListAccountsByItem(ctx, item.ItemID)
		if err != nil {
			slog.Error("failed to list accounts", "item", item.ItemID, "err", err)
			continue
		}
		for _, a := range accounts {
			mask := ""
			if a.Mask != nil && *a.Mask != "" {
				mask = " ••" + html.EscapeString(*a.Mask)
			}
			sb.WriteString(fmt.Sprintf("      • %s%s\n", html.EscapeString(a.Name), mask))
		}
	}
	sb.WriteString("\nTo unlink one, send /unlink followed by its number (e.g. /unlink 1).")
	_, err = b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:    chatID,
		Text:      sb.String(),
		ParseMode: models.ParseModeHTML,
	})
	if err != nil {
		slog.Error("failed to send links list", "chatID", chatID, "err", err)
	}
}

// formatMoney renders an amount for user-facing text, e.g. "$1,234.56";
// non-USD amounts get their ISO code appended.
func formatMoney(amount float64, code *string) string {
	s := formatDollars(math.Abs(amount))
	if amount < 0 {
		s = "-" + s
	}
	if code != nil && *code != "" && *code != "USD" {
		s += " " + *code
	}
	return s
}

// handleBalance shows the balances of every linked account, grouped by
// institution. Balances are whatever the last sync wrote, so each group
// carries its item's sync time.
func (t *TelegramBot) handleBalance(ctx context.Context, b *bot.Bot, update *models.Update) {
	user := UserFromContext(ctx)
	chatID := update.Message.Chat.ID
	items, err := t.store.ListItemsByUser(ctx, user.ID())
	if err != nil {
		slog.Error("failed to list items", "chatID", chatID, "err", err)
		t.sendText(ctx, b, chatID, errTryLater)
		return
	}
	if len(items) == 0 {
		t.sendText(ctx, b, chatID, "You have no linked accounts. Use /link to connect one.")
		return
	}
	var sb strings.Builder
	sb.WriteString("💰 <b>Account balances</b>\n")
	for _, item := range items {
		sb.WriteString(fmt.Sprintf("\n<b>%s</b>",
			html.EscapeString(stringOr(item.InstitutionName, "Unknown institution"))))
		if item.LastSyncedAt != nil {
			sb.WriteString(fmt.Sprintf("  <i>(synced %s)</i>", item.LastSyncedAt.Local().Format("Jan 2, 3:04 PM")))
		}
		sb.WriteString("\n")
		accounts, err := t.store.ListAccountsByItem(ctx, item.ItemID)
		if err != nil {
			slog.Error("failed to list accounts", "item", item.ItemID, "err", err)
			continue
		}
		for _, a := range accounts {
			label := html.EscapeString(a.Name)
			if a.Mask != nil && *a.Mask != "" {
				label += " ••" + html.EscapeString(*a.Mask)
			}
			// Credit balances are money owed, not money held.
			emoji := "💵"
			if a.Type != nil && *a.Type == "credit" {
				emoji = "💳"
			}
			balance := "—"
			if a.BalanceCurrent != nil {
				balance = "<b>" + formatMoney(*a.BalanceCurrent, a.IsoCurrencyCode) + "</b>"
				if a.Type != nil && *a.Type == "credit" {
					balance += " owed"
				}
			}
			sb.WriteString(fmt.Sprintf("      %s %s: %s", emoji, label, balance))
			if a.BalanceAvailable != nil && (a.BalanceCurrent == nil || *a.BalanceAvailable != *a.BalanceCurrent) {
				sb.WriteString(fmt.Sprintf(" (%s available)", formatMoney(*a.BalanceAvailable, a.IsoCurrencyCode)))
			}
			sb.WriteString("\n")
		}
	}
	_, err = b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:    chatID,
		Text:      sb.String(),
		ParseMode: models.ParseModeHTML,
	})
	if err != nil {
		slog.Error("failed to send balance list", "chatID", chatID, "err", err)
	}
}

// handleUnlink resolves /unlink <n> to an item and asks for confirmation;
// the actual removal happens in handleUnlinkCallback.
func (t *TelegramBot) handleUnlink(ctx context.Context, b *bot.Bot, update *models.Update) {
	user := UserFromContext(ctx)
	chatID := update.Message.Chat.ID
	parts := strings.Fields(update.Message.Text)
	if len(parts) != 2 {
		t.sendText(ctx, b, chatID,
			"To unlink, send /unlink followed by the account number from /links (e.g. /unlink 1).")
		return
	}
	items, err := t.store.ListItemsByUser(ctx, user.ID())
	if err != nil {
		slog.Error("failed to list items", "chatID", chatID, "err", err)
		t.sendText(ctx, b, chatID, errTryLater)
		return
	}
	// /links displays 1-based numbers, so translate back to a slice index.
	n, err := strconv.Atoi(parts[1])
	if err != nil || n < 1 || n > len(items) {
		t.sendText(ctx, b, chatID,
			"That doesn't match one of your account numbers — check /links and try again.")
		return
	}
	item := items[n-1]
	inst := html.EscapeString(stringOr(item.InstitutionName, "Unknown institution"))
	_, err = b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID,
		Text: fmt.Sprintf("Unlink <b>%s</b>?\n\nThis disconnects it at Plaid and deletes its accounts "+
			"and transaction history here. It cannot be undone.", inst),
		ParseMode: models.ParseModeHTML,
		ReplyMarkup: &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{{
				{Text: "🔌 Unlink", CallbackData: "unlink:yes:" + item.ItemID},
				{Text: "✖️ Keep", CallbackData: "unlink:no:" + item.ItemID},
			}},
		},
	})
	if err != nil {
		slog.Error("failed to send unlink confirmation", "chatID", chatID, "err", err)
	}
}

func (t *TelegramBot) handleUnlinkCallback(ctx context.Context, b *bot.Bot, update *models.Update) {
	user := UserFromContext(ctx)
	parts := strings.SplitN(update.CallbackQuery.Data, ":", 3)
	if len(parts) != 3 {
		t.answerCallback(ctx, b, update, "Something went wrong")
		return
	}
	choice, itemID := parts[1], parts[2]
	if choice == "no" {
		t.editCallbackMessage(ctx, b, update, "✖️ Kept — the account stays linked.")
		t.answerCallback(ctx, b, update, "")
		return
	}
	item, err := t.store.GetItemByID(ctx, itemID)
	if err != nil {
		slog.Error("unlink for unknown item", "item", itemID, "err", err)
		t.editCallbackMessage(ctx, b, update, "This account is already unlinked.")
		t.answerCallback(ctx, b, update, "")
		return
	}
	// Callback data is client-supplied; never unlink an item the sender
	// doesn't own.
	if item.UserID != user.ID() {
		t.answerCallback(ctx, b, update, "Not authorized")
		return
	}
	inst := html.EscapeString(stringOr(item.InstitutionName, "Unknown institution"))
	if err := t.removeItem(ctx, item); err != nil {
		slog.Error("failed to unlink item", "item", itemID, "err", err)
		t.answerCallback(ctx, b, update, "Something went wrong — the account is still linked. Try again later.")
		return
	}
	slog.Info("item unlinked", "item", itemID, "chatID", user.TelegramID())
	t.editCallbackMessage(ctx, b, update, fmt.Sprintf("🔌 Unlinked <b>%s</b> and deleted its data.", inst))
	t.answerCallback(ctx, b, update, "Unlinked")
}

// handleUnregister asks for confirmation before the full wipe in
// handleUnregisterCallback.
func (t *TelegramBot) handleUnregister(ctx context.Context, b *bot.Bot, update *models.Update) {
	chatID := update.Message.Chat.ID
	_, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID,
		Text: "⚠️ <b>Unregister from Runway?</b>\n\nThis unlinks all of your accounts from Plaid and " +
			"permanently deletes your data here — transactions, accounts, and your registration. " +
			"It cannot be undone, and you'd need a new invite code to come back.",
		ParseMode: models.ParseModeHTML,
		ReplyMarkup: &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{{
				{Text: "💥 Delete everything", CallbackData: "unreg:yes"},
				{Text: "✖️ Stay registered", CallbackData: "unreg:no"},
			}},
		},
	})
	if err != nil {
		slog.Error("failed to send unregister confirmation", "chatID", chatID, "err", err)
	}
}

func (t *TelegramBot) handleUnregisterCallback(ctx context.Context, b *bot.Bot, update *models.Update) {
	user := UserFromContext(ctx)
	if update.CallbackQuery.Data != "unreg:yes" {
		t.editCallbackMessage(ctx, b, update, "✖️ Cancelled — your account is safe.")
		t.answerCallback(ctx, b, update, "")
		return
	}
	items, err := t.store.ListItemsByUser(ctx, user.ID())
	if err != nil {
		slog.Error("failed to list items for unregister", "chatID", user.TelegramID(), "err", err)
		t.answerCallback(ctx, b, update, "Something went wrong — nothing was deleted. Try again later.")
		return
	}
	// Items are removed one at a time; a failure mid-way leaves the rest
	// linked and the user's row intact, so /unregister can simply be re-run.
	for _, item := range items {
		if err := t.removeItem(ctx, item); err != nil {
			slog.Error("failed to remove item during unregister", "item", item.ItemID, "err", err)
			t.answerCallback(ctx, b, update, "Something went wrong part-way — some accounts may remain. Run /unregister again later.")
			return
		}
	}
	if err := t.store.DeleteUser(ctx, user.ID()); err != nil {
		slog.Error("failed to delete user", "chatID", user.TelegramID(), "err", err)
		t.answerCallback(ctx, b, update, "Something went wrong — your accounts are unlinked but registration remains. Run /unregister again later.")
		return
	}
	slog.Info("user unregistered", "chatID", user.TelegramID())
	chatID := user.TelegramID()
	if err := t.setCommandMenu(ctx, chatID, nil); err != nil {
		slog.Error("failed to reset command menu", "chatID", chatID, "err", err)
	}
	t.editCallbackMessage(ctx, b, update,
		"👋 You're unregistered and your data has been deleted. Thanks for trying Runway — "+
			"if you ever want back in, you'll need a new invite code.")
	t.answerCallback(ctx, b, update, "")
}

// removeItem disconnects the item at Plaid, then deletes its local data.
// Plaid removal goes first: if it fails we keep our rows so the user can
// retry, instead of orphaning a live token we no longer have a record of.
func (t *TelegramBot) removeItem(ctx context.Context, item sqlcgen.Item) error {
	accessToken, err := DecryptColumnSecret(item.AccessToken, item.ItemID, t.cfg.DBCryptKey)
	if err != nil {
		return fmt.Errorf("decrypt access token: %w", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, t.cfg.PlaidTimeout)
	defer cancel()
	req := plaid.NewItemRemoveRequest(accessToken)
	if _, _, err := t.plaid.PlaidApi.ItemRemove(callCtx).ItemRemoveRequest(*req).Execute(); err != nil {
		return fmt.Errorf("plaid item remove: %w", err)
	}
	return t.store.ExecTx(ctx, func(q *sqlcgen.Queries) error {
		if err := q.DeleteTransactionsByItem(ctx, item.ItemID); err != nil {
			return fmt.Errorf("delete transactions: %w", err)
		}
		if err := q.DeleteAccountsByItem(ctx, item.ItemID); err != nil {
			return fmt.Errorf("delete accounts: %w", err)
		}
		if err := q.DeleteItem(ctx, item.ItemID); err != nil {
			return fmt.Errorf("delete item: %w", err)
		}
		return nil
	})
}

// editCallbackMessage rewrites the confirmation message a callback came from
// (e.g. replacing the Unlink/Keep prompt with the outcome), dropping its
// keyboard so the buttons can't be tapped twice.
func (t *TelegramBot) editCallbackMessage(ctx context.Context, b *bot.Bot, update *models.Update, text string) {
	msg := update.CallbackQuery.Message.Message
	if msg == nil {
		return
	}
	_, err := b.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID:    msg.Chat.ID,
		MessageID: msg.ID,
		Text:      text,
		ParseMode: models.ParseModeHTML,
	})
	if err != nil {
		slog.Error("failed to edit callback message", "err", err)
	}
}
