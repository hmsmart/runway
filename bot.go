package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/google/uuid"
	"github.com/hmsmart/runway/database"
	"github.com/hmsmart/runway/database/sqlcgen"
	"github.com/hmsmart/runway/domains"
	"github.com/plaid/plaid-go/v43/plaid"
)

// inviteCodePattern matches a normalized invite code: 8 base32 characters,
// uppercased with dashes stripped.
var inviteCodePattern = regexp.MustCompile(`^[A-Z2-7]{8}$`)

// Notification policy: transactions older than the window are never
// announced; within the window, anything older than the fresh cutoff is
// delivered silently (no sound/banner) so backfills don't melt phones.
// Sends are paced 1–3s apart per chat, under Telegram's 1 msg/sec limit.
const (
	notifyWindowDays  = 30
	freshCutoffDays   = 3
	minNotifyPause    = time.Second
	notifyPauseJitter = 2 * time.Second
	avgNotifyPause    = minNotifyPause + notifyPauseJitter/2 // for ETA copy
)

type TelegramBot struct {
	bot   *bot.Bot
	cfg   *Config
	store *database.Store
	plaid *plaid.APIClient
	// runCtx bounds the background drain workers; it should be the app's
	// run context so workers stop on shutdown.
	runCtx context.Context
	// drains maps chat ID -> kick channel (buffered, size 1) for that
	// chat's drain worker. Workers spawn lazily and park forever.
	drains     sync.Map
	thresholds *thresholdTracker
}

func NewTelegramBot(ctx context.Context, cfg *Config, store *database.Store, plaidClient *plaid.APIClient) (*TelegramBot, error) {
	b, err := bot.New(cfg.TGBotKey)
	if err != nil {
		return nil, fmt.Errorf("create telegram bot: %w", err)
	}
	return &TelegramBot{bot: b, cfg: cfg, store: store, plaid: plaidClient, runCtx: ctx, thresholds: newThresholdTracker()}, nil
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
			// Scan can fail mid-row (e.g. a malformed column value) and leave
			// the earlier fields populated; don't act on a half-read user.
			row = sqlcgen.User{}
		} else {
			slog.Info("checking for existing user profile", "chatID", chatID, "userID", row.ID)
			has := t.store.TGPhotos.Has(row.ID)
			if !has {
				slog.Info("i don't have it going to fetch it", "chatID", chatID, "userID", row.ID)
				pic := t.fetchUserPhoto(ctx, *row.TgID)
				t.store.TGPhotos.Set(row.ID, *pic, 24*time.Hour)

			}
		}
		// Callback updates have no Message, so only messages carry a sender
		// name to stamp onto the row.
		if update.Message != nil && update.Message.From != nil {
			row.TgFirstName = &update.Message.From.FirstName
		}
		next(domains.WithUser(ctx, domains.NewUser(row)), b, update)
	}
}

// syncCommands refreshes the per-chat command menu to match the context
// user's state (registered, active, can invite). Callback updates pass
// straight through: they have no message sender and fire on every button tap.
func (t *TelegramBot) syncCommands(next bot.HandlerFunc) bot.HandlerFunc {
	return func(ctx context.Context, b *bot.Bot, update *models.Update) {
		if update.Message != nil {
			if err := t.setCommandMenu(ctx, update.Message.Chat.ID, domains.UserFromContext(ctx)); err != nil {
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
			user := domains.UserFromContext(ctx)
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
	if update.Message == nil {
		return
	}
	// Nil means genuinely unregistered; anyone else who fails a permission
	// check (e.g. /invite without invite rights) is already registered, so
	// telling them to /register would just send them in a circle.
	if domains.UserFromContext(ctx) == nil {
		t.sendText(ctx, b, update.Message.Chat.ID,
			"You're not authorized. If you have an invite code, send <code>/register</code> followed by the code to get started.")
		return
	}
	t.sendText(ctx, b, update.Message.Chat.ID, "You don't have permission to do that.")
}

// sendText sends an HTML-formatted message and logs delivery failures. Every
// caller's text is a static string we wrote ourselves (command mentions
// wrapped in <code>), never unescaped user input — callers embedding
// arbitrary content must escape it or call SendMessage directly instead.
func (t *TelegramBot) sendText(ctx context.Context, b *bot.Bot, chatID int64, text string) {
	_, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:    chatID,
		Text:      text,
		ParseMode: models.ParseModeHTML,
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
	t.bot.RegisterHandler(bot.HandlerTypeMessageText, "/help", bot.MatchTypeExact,
		chain(t.handleHelp, t.fetchUser, t.syncCommands))
	t.bot.RegisterHandler(bot.HandlerTypeMessageText, "/budget", bot.MatchTypePrefix,
		chain(t.handleBudget, t.fetchUser, t.syncCommands, t.requirePermission(domains.PermissionActive)))
	t.bot.RegisterHandler(bot.HandlerTypeMessageText, "/runway", bot.MatchTypeExact,
		chain(t.handleRunway, t.fetchUser, t.syncCommands, t.requirePermission(domains.PermissionActive)))
	t.bot.RegisterHandler(bot.HandlerTypeMessageText, "/report", bot.MatchTypePrefix,
		chain(t.handleReport, t.fetchUser, t.syncCommands, t.requirePermission(domains.PermissionActive)))
	t.bot.RegisterHandlerMatchFunc(func(update *models.Update) bool {
		return update.Message != nil && update.Message.Photo != nil
	}, chain(t.handleReceipt, t.fetchUser, t.requirePermission(domains.PermissionActive)))
	t.bot.RegisterHandler(bot.HandlerTypeMessageText, "/link", bot.MatchTypeExact,
		chain(t.handleLink, t.fetchUser, t.syncCommands, t.requirePermission(domains.PermissionActive)))
	t.bot.RegisterHandler(bot.HandlerTypeMessageText, "/dash", bot.MatchTypeExact,
		chain(t.handleDash, t.fetchUser, t.syncCommands, t.requirePermission(domains.PermissionActive)))
	t.bot.RegisterHandler(bot.HandlerTypeMessageText, "/cash", bot.MatchTypePrefix,
		chain(t.handleCash, t.fetchUser, t.syncCommands, t.requirePermission(domains.PermissionActive)))
	t.bot.RegisterHandler(bot.HandlerTypeMessageText, "/hold", bot.MatchTypePrefix,
		chain(t.handleHold, t.fetchUser, t.syncCommands, t.requirePermission(domains.PermissionActive)))
	t.bot.RegisterHandler(bot.HandlerTypeMessageText, "/apikey", bot.MatchTypeExact,
		chain(t.handleAPIKey, t.fetchUser, t.syncCommands, t.requirePermission(domains.PermissionActive)))
	// /receipt is a discoverability stub: photos book themselves (see the
	// MatchFunc above — photo messages carry a Caption, not Text, so the two
	// never collide), and this just tells the user that.
	t.bot.RegisterHandler(bot.HandlerTypeMessageText, "/receipt", bot.MatchTypePrefix,
		chain(t.handleReceiptHelp, t.fetchUser, t.syncCommands, t.requirePermission(domains.PermissionActive)))
	t.bot.RegisterHandler(bot.HandlerTypeMessageText, "/accounts", bot.MatchTypeExact,
		chain(t.handleAccounts, t.fetchUser, t.syncCommands, t.requirePermission(domains.PermissionActive)))
	// Exact must be registered before Prefix: handlers are tried in order and
	// the first match wins, so bare "/unlink" hits this one and shows the
	// account list, while "/unlink 1" fails the exact match and falls
	// through to the prefix handler below.
	t.bot.RegisterHandler(bot.HandlerTypeMessageText, "/unlink", bot.MatchTypeExact,
		chain(t.handleAccounts, t.fetchUser, t.syncCommands, t.requirePermission(domains.PermissionActive)))
	t.bot.RegisterHandler(bot.HandlerTypeMessageText, "/unlink", bot.MatchTypePrefix,
		chain(t.handleUnlink, t.fetchUser, t.syncCommands, t.requirePermission(domains.PermissionActive)))
	t.bot.RegisterHandler(bot.HandlerTypeMessageText, "/unregister", bot.MatchTypeExact,
		chain(t.handleUnregister, t.fetchUser, t.syncCommands, t.requirePermission(domains.PermissionActive)))
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
	t.bot.RegisterHandler(bot.HandlerTypeCallbackQueryData, "merge:", bot.MatchTypePrefix,
		chain(t.handleMerge, t.fetchUser, t.requirePermission(domains.PermissionActive)))
	t.bot.RegisterHandler(bot.HandlerTypeCallbackQueryData, "mortize:", bot.MatchTypePrefix,
		chain(t.handleMortize, t.fetchUser, t.requirePermission(domains.PermissionActive)))
	t.bot.RegisterHandler(bot.HandlerTypeCallbackQueryData, "unlink:", bot.MatchTypePrefix,
		chain(t.handleUnlinkCallback, t.fetchUser, t.requirePermission(domains.PermissionActive)))
	t.bot.RegisterHandler(bot.HandlerTypeCallbackQueryData, "unreg:", bot.MatchTypePrefix,
		chain(t.handleUnregisterCallback, t.fetchUser, t.requirePermission(domains.PermissionActive)))
}

func (t *TelegramBot) handleStart(ctx context.Context, b *bot.Bot, update *models.Update) {
	slog.Info("called start", "chatID", update.Message.Chat.ID)
	user := domains.UserFromContext(ctx)
	if user.Has(domains.PermissionActive) {
		if user.Discretionary() == nil {
			t.sendText(ctx, b, update.Message.Chat.ID,
				"Welcome back to Runway! Before you can link an account, I need your monthly discretionary budget.\n\n"+budgetExplainer)
			return
		}
		t.sendText(ctx, b, update.Message.Chat.ID,
			"Welcome back to Runway! You're all set up — <code>/link</code> to connect another account, <code>/accounts</code> to check your "+
				"accounts and balances, or <code>/budget</code> to adjust your monthly discretionary budget. <code>/help</code> lists everything I can do.")
		return
	}
	t.sendText(ctx, b, update.Message.Chat.ID,
		"Welcome to Runway! I help you keep your monthly discretionary spending — the stuff you choose to spend, "+
			"like dining out or shopping, not rent or bills — under a budget you set.\n\n"+
			"Runway is invite-only, so you'll need an invite code from an existing user. Once you have one:\n"+
			"1. <code>/register</code> the code\n"+
			"2. <code>/budget</code> to set your monthly discretionary budget\n"+
			"3. <code>/link</code> to connect a bank account\n\n"+
			"There's no rush — whenever you have a code, send <code>/register</code> followed by it (e.g. <code>/register ABCD2234</code>). "+
			"<code>/help</code> explains more.")
}

// handleHelp explains what Runway does and which commands are available,
// tailored to how far along the sender is: an unregistered sender gets the
// pitch and setup steps, an active user gets the full command reference plus
// what the Spread/Exclude transaction buttons do.
func (t *TelegramBot) handleHelp(ctx context.Context, b *bot.Bot, update *models.Update) {
	user := domains.UserFromContext(ctx)
	chatID := update.Message.Chat.ID
	var text string
	switch {
	case !user.Has(domains.PermissionActive):
		text = "🧭 <b>Runway</b> tracks your monthly discretionary spending — the stuff you choose to spend " +
			"(dining out, shopping, fun), not fixed bills like rent or insurance.\n\n" +
			"Getting started:\n" +
			"1. <code>/register CODE</code> — join with an invite code\n" +
			"2. <code>/budget AMOUNT</code> — set your monthly discretionary budget\n" +
			"3. <code>/link</code> — connect a bank account\n\n" +
			"<code>/ping</code> — check that I'm up"
	case user.Discretionary() == nil:
		text = "🧭 <b>Runway commands</b>\n\n" +
			"<code>/budget AMOUNT</code> — set your monthly discretionary budget (required before linking)\n" +
			"<code>/unregister</code> — delete your data and leave\n\n" +
			"Once your budget is set, <code>/link</code> unlocks so you can connect a bank account."
	default:
		text = "🧭 <b>Runway commands</b>\n\n" +
			"<code>/runway</code> — yesterday's spend, your daily rates, and days of cash left\n" +
			"<code>/report [TIME]</code> — get the runway report every day at TIME (e.g. <code>/report 8:00</code>); <code>/report off</code> stops it\n" +
			"<code>/budget [AMOUNT]</code> — view or update your monthly discretionary budget\n" +
			"<code>/cash AMOUNT NOTE</code> — jot down a cash transaction (e.g. <code>/cash 5.50 taco truck</code>)\n" +
			"<code>/hold AMOUNT [NAME]</code> — hold a card swipe until Plaid confirms it (e.g. <code>/hold 12.50 chipotle</code>)\n" +
			"📸 or just send a photo of a receipt and I'll book it as a transaction\n" +
			"<code>/dash</code> — sign in to your web dashboard\n" +
			"<code>/link</code> — connect a bank account\n" +
			"<code>/accounts</code> — list your linked accounts and balances\n" +
			"<code>/unlink [N]</code> — list your accounts, or unlink one by number\n"
		text += "<code>/apikey</code> — generate an API key for iOS Shortcuts\n"
		if user.Has(domains.PermissionInvite) {
			text += "<code>/invite</code> — create an invite code for a new user\n"
		}
		text += "<code>/unregister</code> — delete your data and leave\n\n" +
			"Each transaction I send has two buttons:\n" +
			"📊 <b>Spread</b> — spreads a big one-off purchase across weeks or months so it doesn't blow your budget in a single day\n" +
			"🚫 <b>Exclude</b> — leaves it out of your discretionary spend entirely (handy for a rent payment or transfer that slipped through)"
	}
	_, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:    chatID,
		Text:      text,
		ParseMode: models.ParseModeHTML,
	})
	if err != nil {
		slog.Error("failed to send help", "chatID", chatID, "err", err)
	}
}

func (t *TelegramBot) handlePing(ctx context.Context, b *bot.Bot, update *models.Update) {
	user := domains.UserFromContext(ctx)
	slog.Info("called ping", "chatID", update.Message.Chat.ID, "registered", user != nil)
	pong := "Pongthenticated"
	if user == nil {
		pong = "Unpongthenticated"
	}
	t.sendText(ctx, b, update.Message.Chat.ID, pong)
}

// handleReceiptHelp explains the receipt-photo capability. The command
// exists so the feature shows in the menu; the actual trigger is simply
// sending a photo.
func (t *TelegramBot) handleReceiptHelp(ctx context.Context, b *bot.Bot, update *models.Update) {
	t.sendText(ctx, b, update.Message.Chat.ID,
		"📸 Just send me a photo of a receipt — no command needed — and I'll book it as a transaction. "+
			"Add a caption if anything needs clarifying (the total, the store, the date).\n\n"+
			"For quick text-only entries, use <code>/cash</code>, e.g. <code>/cash 5.50 taco truck</code>.")
}

// handleCash books a quick text note ("/cash 5.50 taco truck") as a manual
// transaction. Same reaction protocol as handleReceipt: 👨‍💻 while the model
// works, 👍 once booked, 👎 plus a reply on failure.
func (t *TelegramBot) handleCash(ctx context.Context, b *bot.Bot, update *models.Update) {
	user := domains.UserFromContext(ctx)
	chatID := update.Message.Chat.ID
	msgID := update.Message.ID
	slog.Info("parsing cash transaction for user", "chatID", chatID)
	fail := func(text string) {
		t.react(ctx, chatID, msgID, "👎")
		t.replyText(ctx, b, chatID, msgID, text)
	}
	userContext := strings.TrimSpace(strings.TrimPrefix(update.Message.Text, "/cash"))
	if userContext == "" {
		fail("Jot the transaction after the command, e.g. <code>/cash 5.50 taco truck</code>. Amounts, store names, and dates all help.")
		return
	}
	t.react(ctx, chatID, msgID, "👨‍💻")
	receiptJSON, err := processReceiptNote(ctx, userContext, t.cfg)
	if err != nil {
		slog.Error("failed to process receipt note", "chatID", chatID, "err", err)
		fail("failed to process that note, please try again later")
		return
	}
	rt, err := parseReceiptTransaction(receiptJSON)
	if err != nil {
		slog.Error("model returned unusable receipt json", "chatID", chatID, "json", receiptJSON, "err", err)
		fail(receiptParseErrorText(err, "Try adding more detail (e.g. the total and store name)."))
		return
	}
	txID, err := createManualTransaction(ctx, t.store, t.cfg, user.ID(), rt, receiptJSON)
	if err != nil {
		slog.Error("failed to create manual transaction", "chatID", chatID, "err", err)
		fail(errTryLater)
		return
	}
	slog.Info("created manual transaction", "chatID", chatID, "tx", txID, "amt", rt.Amount, "date", rt.Date)
	t.react(ctx, chatID, msgID, "👍")
	t.recomputeSpend(ctx)
	// The row landed at notified = 0; the drain worker announces it with the
	// usual Spread/Exclude buttons.
	t.startDrain(chatID)
}

func (t *TelegramBot) handleAPIKey(ctx context.Context, b *bot.Bot, update *models.Update) {
	user := domains.UserFromContext(ctx)
	chatID := update.Message.Chat.ID
	key := RandomToken(24, Base64)
	if err := t.store.SetUserAPIKey(ctx, sqlcgen.SetUserAPIKeyParams{
		ApiKey: &key,
		ID:     user.ID(),
	}); err != nil {
		slog.Error("failed to set api key", "chatID", chatID, "err", err)
		t.sendText(ctx, b, chatID, errTryLater)
		return
	}
	t.sendText(ctx, b, chatID,
		"🔑 Your new API key (replaces any previous one):\n\n"+
			"<code>"+key+"</code>\n\n"+
			"POST to <code>"+t.cfg.BaseURL+"/hook/hold</code> with header:\n"+
			"<code>Authorization: Bearer YOUR_KEY</code>\n\n"+
			"Body:\n"+
			"<pre>{\"Amount\":\"12.50\",\"Merchant\":\"Store\",\"Name\":\"Card\"}</pre>")
}

func (t *TelegramBot) handleHold(ctx context.Context, b *bot.Bot, update *models.Update) {
	user := domains.UserFromContext(ctx)
	chatID := update.Message.Chat.ID
	msgID := update.Message.ID
	fail := func(text string) {
		t.react(ctx, chatID, msgID, "👎")
		t.replyText(ctx, b, chatID, msgID, text)
	}
	raw := strings.TrimSpace(strings.TrimPrefix(update.Message.Text, "/hold"))
	if raw == "" {
		fail("Usage: <code>/hold 12.50</code> or <code>/hold 12.50 chipotle</code>")
		return
	}
	parts := strings.SplitN(raw, " ", 2)
	amt, err := parseHoldAmount(parts[0])
	if err != nil {
		fail("Couldn't parse that amount. Try <code>/hold 12.50</code>")
		return
	}
	merchant := "Card Transaction"
	if len(parts) > 1 {
		merchant = strings.TrimSpace(parts[1])
	}
	txID, err := createHoldTransaction(ctx, t.store, t.cfg, user.ID(), amt, merchant)
	if err != nil {
		slog.Error("failed to create hold", "chatID", chatID, "err", err)
		fail(errTryLater)
		return
	}
	slog.Info("created hold via command", "chatID", chatID, "tx", txID, "amt", amt, "merchant", merchant)
	t.react(ctx, chatID, msgID, "👍")
	t.recomputeSpend(ctx)
	t.startDrain(chatID)
}

// receiptParseErrorText maps a parseReceiptTransaction error to user-facing
// copy: a too-old date is the user's to fix (and gets an explanation),
// anything else gets the generic retry line plus the caller's hint.
func receiptParseErrorText(err error, hint string) string {
	if errors.Is(err, errReceiptTooOld) {
		return fmt.Sprintf("That transaction is dated more than %d days back — older than what I track, so I can't book it.", notifyWindowDays)
	}
	return "I couldn't make sense of that receipt. " + hint
}

// react sets (or replaces) the bot's emoji reaction on a message. The emoji
// must be on Telegram's allowed-reaction list. Reactions are the receipt
// flow's status channel: 👨‍💻 working, 👍 booked, 👎 failed.
func (t *TelegramBot) react(ctx context.Context, chatID int64, messageID int, emoji string) {
	_, err := t.bot.SetMessageReaction(ctx, &bot.SetMessageReactionParams{
		ChatID:    chatID,
		MessageID: messageID,
		Reaction: []models.ReactionType{{
			Type: models.ReactionTypeTypeEmoji,
			ReactionTypeEmoji: &models.ReactionTypeEmoji{
				Type:  models.ReactionTypeTypeEmoji,
				Emoji: emoji,
			},
		}},
	})
	if err != nil {
		slog.Error("failed to set message reaction", "chatID", chatID, "msg", messageID, "err", err)
	}
}

// replyText sends an HTML-formatted reply pinned to a specific message.
// Same static-string contract as sendText.
func (t *TelegramBot) replyText(ctx context.Context, b *bot.Bot, chatID int64, messageID int, text string) {
	_, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:          chatID,
		Text:            text,
		ParseMode:       models.ParseModeHTML,
		ReplyParameters: &models.ReplyParameters{MessageID: messageID},
	})
	if err != nil {
		slog.Error("failed to send reply", "chatID", chatID, "msg", messageID, "err", err)
	}
}

// handleReceipt books a photographed receipt as a manual transaction. Status
// is reported through reactions on the photo itself — 👨‍💻 while the vision
// model works, 👍 once the transaction is booked (the announcement card
// follows via the drain worker), 👎 plus an explanatory reply on failure —
// so the flow adds no chat clutter beyond the transaction card.
func (t *TelegramBot) handleReceipt(ctx context.Context, b *bot.Bot, update *models.Update) {
	user := domains.UserFromContext(ctx)
	chatID := update.Message.Chat.ID
	msgID := update.Message.ID
	t.react(ctx, chatID, msgID, "👨‍💻")
	userInput := strings.TrimSpace(update.Message.Caption)
	// fail marks the photo 👎 and explains in a reply pinned to it.
	fail := func(text string) {
		t.react(ctx, chatID, msgID, "👎")
		t.replyText(ctx, b, chatID, msgID, text)
	}
	photo := update.Message.Photo
	if len(photo) == 0 {
		slog.Error("expected a photo but got an empty array", "chatID", chatID)
		fail("failed to process attachment, please try again later")
		return
	}
	// Sizes come back smallest-first; the last is the full-resolution photo.
	lgPhoto := photo[len(photo)-1]
	file, err := t.bot.GetFile(ctx, &bot.GetFileParams{FileID: lgPhoto.FileID})
	if err != nil {
		slog.Error("failed to get attachment file url", "chatID", chatID, "err", err)
		fail("failed to process attachment, please try again later")
		return
	}
	resp, err := http.Get(t.bot.FileDownloadLink(file))
	if err != nil {
		slog.Error("failed to download attachment", "chatID", chatID, "err", redactURLError(err))
		fail("failed to process attachment, please try again later")
		return
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxReceiptPhotoBytes+1))
	if err != nil {
		slog.Error("failed to read attachment body", "chatID", chatID, "err", err)
		fail("failed to process attachment, please try again later")
		return
	}
	receiptJSON, err := processReceiptImage(ctx, data, userInput, t.cfg)
	if err != nil {
		slog.Error("failed to process receipt image", "chatID", chatID, "err", err)
		fail("failed to read that receipt, please try again later")
		return
	}
	rt, err := parseReceiptTransaction(receiptJSON)
	if err != nil {
		slog.Error("model returned unusable receipt json", "chatID", chatID, "json", receiptJSON, "err", err)
		fail(receiptParseErrorText(err, "Try a clearer photo, or add context in the caption (e.g. the total and store name)."))
		return
	}
	txID, err := createManualTransaction(ctx, t.store, t.cfg, user.ID(), rt, receiptJSON)
	if err != nil {
		slog.Error("failed to create manual transaction", "chatID", chatID, "err", err)
		fail(errTryLater)
		return
	}
	slog.Info("created manual transaction", "chatID", chatID, "tx", txID, "amt", rt.Amount)
	t.react(ctx, chatID, msgID, "👍")
	t.recomputeSpend(ctx)
	// The row landed at notified = 0; the drain worker announces it with the
	// usual Spread/Exclude buttons.
	t.startDrain(chatID)
}

func (t *TelegramBot) handleRegistration(ctx context.Context, b *bot.Bot, update *models.Update) {
	parts := strings.Fields(update.Message.Text)
	if len(parts) != 2 {
		slog.Info("invalid arguments passed", "chatID", update.Message.Chat.ID, "message", update.Message.Text)
		t.sendText(ctx, b, update.Message.Chat.ID,
			"To register, send <code>/register</code> followed by your invite code (e.g. <code>/register ABCD2234</code>).")
		return
	}
	regcode := strings.ToUpper(parts[1])
	regcode = strings.ReplaceAll(regcode, "-", "")
	if !inviteCodePattern.MatchString(regcode) {
		slog.Info("failed to validate code", "chatID", update.Message.Chat.ID, "code", regcode)
		t.sendText(ctx, b, update.Message.Chat.ID,
			"That invite code doesn't look right. Codes are 8 letters and digits, e.g. ABCD2234.")
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
			"Welcome to Runway! You're registered.\n\n"+
				"First, let's set your monthly discretionary budget — that's the number I'll track your spending against.\n\n"+budgetExplainer)
	}
	chain(welcome, t.fetchUser, t.syncCommands)(ctx, b, update)
}

// budgetExplainer coaches the user on what counts as discretionary and how to
// set it. Runway only tracks controllable spending, so fixed obligations stay
// out of the number.
const budgetExplainer = "Think about what you spend in a typical month on things you choose — dining out, shopping, hobbies, fun. " +
	"Leave out fixed bills like rent or mortgage, insurance, and utilities; Runway only tracks the spending you can actually control.\n\n" +
	"When you have a number, send <code>/budget</code> followed by the amount (e.g. <code>/budget 1500</code>)."

// parseDollarString turns user input like "1500", "$1,500" or "1500.50" into a
// dollar amount, rejecting non-positive and absurd values.
func parseDollarString(s string) (float64, error) {
	s = strings.TrimPrefix(s, "$")
	s = strings.ReplaceAll(s, ",", "")
	amt, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	// Truncate to cents so stored budgets stay presentable; validate after,
	// so sub-cent inputs can't round down to zero. The range check is phrased
	// positively because NaN fails every comparison ("/budget nan" parses).
	amt = math.Floor(amt*100) / 100
	if !(amt > 0 && amt < 1_000_000) {
		return 0, fmt.Errorf("amount out of range: %v", amt)
	}
	return amt, nil
}

// handleBudget sets or shows the user's monthly discretionary budget. Setting
// it for the first time completes setup and unlocks /link.
func (t *TelegramBot) handleBudget(ctx context.Context, b *bot.Bot, update *models.Update) {
	user := domains.UserFromContext(ctx)
	slog.Info("called budget", "chatID", update.Message.Chat.ID)
	parts := strings.Fields(update.Message.Text)
	if len(parts) == 1 {
		if cur := user.Discretionary(); cur != nil {
			t.sendText(ctx, b, update.Message.Chat.ID,
				fmt.Sprintf("Your monthly discretionary budget is %s/month. To change it, send <code>/budget</code> followed by the new amount (e.g. <code>/budget 1500</code>).", formatDollars(*cur)))
			return
		}
		t.sendText(ctx, b, update.Message.Chat.ID,
			"You haven't set a monthly discretionary budget yet.\n\n"+budgetExplainer)
		return
	}
	amt, err := parseDollarString(parts[1])
	if len(parts) != 2 || err != nil {
		slog.Info("invalid budget amount", "chatID", update.Message.Chat.ID, "message", update.Message.Text)
		t.sendText(ctx, b, update.Message.Chat.ID,
			"I couldn't read that amount. Send <code>/budget</code> followed by a dollar amount, e.g. <code>/budget 1500</code> or <code>/budget 1,500.50</code>.")
		return
	}
	firstTime := user.Discretionary() == nil
	err = t.store.SetDiscretionary(ctx, sqlcgen.SetDiscretionaryParams{
		DiscretionaryMonthly: &amt,
		ID:                   user.ID(),
	})
	if err != nil {
		slog.Error("failed to set discretionary budget", "chatID", update.Message.Chat.ID, "err", err)
		t.sendText(ctx, b, update.Message.Chat.ID, errTryLater)
		return
	}
	slog.Info("budget set", "chatID", update.Message.Chat.ID, "amount", amt, "firstTime", firstTime)
	confirm := func(ctx context.Context, b *bot.Bot, update *models.Update) {
		text := fmt.Sprintf("Updated! Your monthly discretionary budget is now %s/month.", formatDollars(amt))
		if firstTime {
			text = fmt.Sprintf("Budget set: %s/month of discretionary spending.\n\nNow use <code>/link</code> to connect your first bank account and I'll start tracking against it.", formatDollars(amt))
		}
		t.sendText(ctx, b, update.Message.Chat.ID, text)
	}
	// Reload the user so syncCommands unhides the link commands now that
	// setup is complete, then send the confirmation.
	chain(confirm, t.fetchUser, t.syncCommands)(ctx, b, update)
}

func (t *TelegramBot) handleInvite(ctx context.Context, b *bot.Bot, update *models.Update) {
	invUser := domains.UserFromContext(ctx)
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
	t.sendText(ctx, b, update.Message.Chat.ID,
		fmt.Sprintf("Invite created! Share this invite code with your friend: <code>%s</code>\nThey can redeem it by sending me <code>/register %s</code>", inviteCode, inviteCode))
}
func (t *TelegramBot) handleLink(ctx context.Context, b *bot.Bot, update *models.Update) {
	slog.Info("got link request", "chatID", update.Message.Chat.ID)
	user := domains.UserFromContext(ctx)
	// Linking is gated on a budget: without one there's nothing to track
	// spending against, so finish setup first.
	if user != nil && user.Discretionary() == nil {
		slog.Info("link attempted without budget set", "chatID", update.Message.Chat.ID)
		t.sendText(ctx, b, update.Message.Chat.ID,
			"Almost there — before linking an account, I need your monthly discretionary budget so I have something to track your spending against.\n\n"+budgetExplainer)
		return
	}
	t.sendMagicLink(ctx, b, update.Message.Chat.ID, user, "/link",
		formatLinkMessage(t.cfg.TokenTTL), "🔗 Link Account")
}

// handleDash sends a magic link that signs the user into the web dashboard.
func (t *TelegramBot) handleDash(ctx context.Context, b *bot.Bot, update *models.Update) {
	slog.Info("got dash request", "chatID", update.Message.Chat.ID)
	t.sendMagicLink(ctx, b, update.Message.Chat.ID, domains.UserFromContext(ctx), "/dash",
		formatDashMessage(t.cfg.TokenTTL), "📊 Open Dashboard")
}

// sendMagicLink mints a single-use token identifying user and sends a button
// pointing at the web side's path carrying it. Consuming the token there
// authenticates the browser as this user and starts a session.
func (t *TelegramBot) sendMagicLink(ctx context.Context, b *bot.Bot, chatID int64, user *domains.User, path, text, button string) {
	if user == nil {
		// requirePermission should make this unreachable; never mint a token
		// for nobody.
		slog.Error("magic link requested without user in context", "chatID", chatID, "path", path)
		t.sendText(ctx, b, chatID, errTryLater)
		return
	}
	token := RandomToken(16, Base64)
	t.store.TGTokens.Set(token, *user, t.cfg.TokenTTL)
	params := url.Values{}
	params.Set("token", token)
	_, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID,
		Text:   text,
		ReplyMarkup: &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{{
				{Text: button, URL: t.cfg.BaseURL + path + "?" + params.Encode()},
			}},
		},
		ParseMode: models.ParseModeHTML,
	})
	if err != nil {
		slog.Error("failed to send magic link", "chatID", chatID, "path", path, "err", err)
	}
}

// maxProfilePhotoBytes caps a profile photo download. The smallest Telegram
// size is 160px (a few KB), so the cap only guards against a misbehaving
// response, not legitimate photos.
const maxProfilePhotoBytes = 1 << 20
const maxReceiptPhotoBytes = 20 << 20

// fetchUserPhotoTimeout bounds the whole photo fetch (two API calls plus the
// CDN download). The photo is cosmetic, so a slow fetch should degrade to the
// default photo rather than stall the caller.
const fetchUserPhotoTimeout = 10 * time.Second

// redactURLError strips the request URL from HTTP client errors before they
// are logged: Telegram file download URLs embed the bot token.
func redactURLError(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}
	return err
}

// fetchUserPhoto downloads the user's smallest profile photo. Every failure
// path — no photo set (or hidden by privacy settings), API errors, a bad
// download — logs and falls back to the shared default photo, so callers
// always get a usable *Photo.
func (t *TelegramBot) fetchUserPhoto(ctx context.Context, tgID int64) *domains.Photo {
	ctx, cancel := context.WithTimeout(ctx, fetchUserPhotoTimeout)
	defer cancel()
	photos, err := t.bot.GetUserProfilePhotos(ctx, &bot.GetUserProfilePhotosParams{
		UserID: tgID,
		Limit:  1,
	})
	if err != nil {
		slog.Warn("failed to list profile photos", "chatID", tgID, "err", err)
		return domains.GetDefaultPhoto()
	}
	if len(photos.Photos) == 0 || len(photos.Photos[0]) == 0 {
		slog.Info("user has no profile photo, using default", "chatID", tgID)
		return domains.GetDefaultPhoto()
	}
	// Sizes come back smallest-first; index 0 is the 160px thumbnail.
	smallest := photos.Photos[0][0]
	file, err := t.bot.GetFile(ctx, &bot.GetFileParams{FileID: smallest.FileID})
	if err != nil {
		slog.Warn("failed to resolve profile photo file", "chatID", tgID, "fileID", smallest.FileID, "err", err)
		return domains.GetDefaultPhoto()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.bot.FileDownloadLink(file), nil)
	if err != nil {
		slog.Warn("failed to build profile photo request", "chatID", tgID, "err", redactURLError(err))
		return domains.GetDefaultPhoto()
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Warn("failed to download profile photo", "chatID", tgID, "fileID", smallest.FileID, "err", redactURLError(err))
		return domains.GetDefaultPhoto()
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Warn("profile photo download rejected", "chatID", tgID, "fileID", smallest.FileID, "status", resp.Status)
		return domains.GetDefaultPhoto()
	}
	// Read one byte past the cap so a photo of exactly the limit passes but
	// anything larger is detected as truncated rather than silently clipped.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxProfilePhotoBytes+1))
	if err != nil {
		slog.Warn("failed to read profile photo body", "chatID", tgID, "fileID", smallest.FileID, "err", err)
		return domains.GetDefaultPhoto()
	}
	if len(data) > maxProfilePhotoBytes {
		slog.Warn("profile photo exceeds size cap", "chatID", tgID, "fileID", smallest.FileID, "cap", maxProfilePhotoBytes)
		return domains.GetDefaultPhoto()
	}
	return domains.NewPhoto(uint(smallest.Width), uint(smallest.Height), data, smallest.FileUniqueID)
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
		// The spread menu only offers Unspread when there's an end date
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

// recomputeSpend refreshes the context user's daily-spend series after a
// classification change (exclude/include, spread/unspread). Failures are
// logged, not surfaced: the classification itself already committed, and the
// next sync or hourly sweep will recompute the same series.
func (t *TelegramBot) recomputeSpend(ctx context.Context) {
	user := domains.UserFromContext(ctx)
	if user == nil {
		return
	}
	if err := recomputeDailySpend(ctx, t.store, user.ID()); err != nil {
		slog.Error("failed to recompute daily spend", "user", user.ID(), "err", err)
	}
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
		t.recomputeSpend(ctx)
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
	t.recomputeSpend(ctx)
	t.refreshMessage(ctx, b, update, txID)
	t.answerCallback(ctx, b, update, "Spreading over "+period.label)
}

// handleMortize clears a transaction's amortization end date.
func (t *TelegramBot) handleMortize(ctx context.Context, b *bot.Bot, update *models.Update) {
	txID := strings.TrimPrefix(update.CallbackQuery.Data, "mortize:")
	if err := t.store.ClearAmortEnd(ctx, txID); err != nil {
		slog.Error("failed to clear transaction amortization", "tx", txID, "err", err)
		t.answerCallback(ctx, b, update, "Something went wrong")
		return
	}
	t.recomputeSpend(ctx)
	t.refreshMessage(ctx, b, update, txID)
	t.answerCallback(ctx, b, update, "Spread removed")
}

// setCommandMenu writes the command menu a user should see given their
// permissions. Unregistered (nil) and inactive users get ping and register.
func (t *TelegramBot) setCommandMenu(ctx context.Context, chatID int64, user *domains.User) error {
	cmds := []models.BotCommand{
		{Command: "ping", Description: "Check that Runway is up"},
		{Command: "help", Description: "Show what Runway can do"},
		{Command: "register", Description: "Register with an invite code"},
	}
	if user.Has(domains.PermissionActive) {
		cmds = []models.BotCommand{
			{Command: "ping", Description: "Check that Runway is up"},
			{Command: "help", Description: "Show what Runway can do"},
			{Command: "budget", Description: "Set your monthly discretionary budget"},
		}
		// Until the budget is set, setting it is the user's only real task,
		// so linking and invites stay out of the menu until setup completes.
		// Unregister always shows: the exit should never be hidden. The
		// commands themselves still work if typed — this only trims the menu.
		if user.Discretionary() != nil {
			cmds = append(cmds,
				models.BotCommand{Command: "runway", Description: "Yesterday's spend and days of cash left"},
				models.BotCommand{Command: "report", Description: "Schedule a daily runway report"},
				models.BotCommand{Command: "receipt", Description: "Send a photo of a receipt to book a transaction"},
				models.BotCommand{Command: "cash", Description: "Jot down a transaction, e.g. /cash 5.50 taco truck"},
				models.BotCommand{Command: "hold", Description: "Hold a card swipe, e.g. /hold 12.50 chipotle"},
				models.BotCommand{Command: "dash", Description: "Sign in to your web dashboard"},
				models.BotCommand{Command: "link", Description: "Link a bank account"},
				models.BotCommand{Command: "accounts", Description: "List your linked accounts and balances"},
				models.BotCommand{Command: "unlink", Description: "List accounts, or unlink one by number"},
			)
			cmds = append(cmds, models.BotCommand{Command: "apikey", Description: "Generate an API key for iOS Shortcuts"})
			if user.Has(domains.PermissionInvite) {
				cmds = append(cmds, models.BotCommand{Command: "invite", Description: "Create an invite code for a new user"})
			}
		}
		cmds = append(cmds, models.BotCommand{Command: "unregister", Description: "Delete your data and leave Runway"})
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
// from, e.g. expanding Spread into its period options.
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
		ReplyMarkup: t.transactionKeyboardFor(ctx, tx),
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

// startDrain ensures a drain worker exists for chatID and kicks it. Safe to
// call from any goroutine, any number of times: kicks carry no payload, so
// while the worker is busy a single buffered kick stands in for any burst,
// and the worker re-queries before parking so no rows are ever stranded.
func (t *TelegramBot) startDrain(chatID int64) {
	kick, loaded := t.drains.LoadOrStore(chatID, make(chan struct{}, 1))
	ch := kick.(chan struct{})
	if !loaded {
		go t.drainWorker(chatID, ch)
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (t *TelegramBot) drainWorker(chatID int64, kick chan struct{}) {
	for range kick {
		t.drainChat(t.runCtx, chatID)
	}
}

// drainChat announces this chat's unsent transactions oldest-first, marking
// each row as it goes, and returns once the pending queue is empty. It
// re-queries between batches so rows inserted mid-drain join in date order.
// Each pass first refreshes stale cards (pendings that settled), so an edit
// never lands after the announcement loop has already rendered fresh state.
func (t *TelegramBot) drainChat(ctx context.Context, chatID int64) {
	announced := false
	for {
		if ctx.Err() != nil {
			return
		}
		windowCutoff := time.Now().AddDate(0, 0, -notifyWindowDays).Format(time.DateOnly)
		if err := t.store.MarkUnnotifiableTransactions(ctx, windowCutoff); err != nil {
			slog.Error("failed to retire unnotifiable transactions", "chatID", chatID, "err", err)
			return
		}
		if err := t.store.ExpireStaleHolds(ctx); err != nil {
			slog.Error("failed to expire stale holds", "chatID", chatID, "err", err)
		}
		if !t.sweepStaleMessages(ctx, chatID) {
			return
		}
		rows, err := t.store.GetPendingNotifications(ctx, &chatID)
		if err != nil {
			slog.Error("failed to load pending notifications", "chatID", chatID, "err", err)
			return
		}
		if len(rows) == 0 {
			if announced {
				t.checkSpendThresholdsForChat(ctx, chatID)
			}
			return
		}
		if !announced {
			announced = true
			t.announceBackfill(ctx, chatID, windowCutoff)
		}
		freshCutoff := time.Now().AddDate(0, 0, -freshCutoffDays).Format(time.DateOnly)
		for _, row := range rows {
			tx := row.Transaction
			// Plaid dates are YYYY-MM-DD, so string comparison is date order.
			silent := tx.Date < freshCutoff
			msgID, err := t.sendTransactionNotification(ctx, chatID, tx, silent)
			if err != nil {
				var tooMany *bot.TooManyRequestsError
				if errors.As(err, &tooMany) {
					// Rate limited: the row stays pending; wait as told and
					// restart the pass so it retries in order.
					if !sleepCtx(ctx, time.Duration(tooMany.RetryAfter)*time.Second) {
						return
					}
					break
				}
				// Anything else is treated as permanent (blocked bot, bad
				// chat): log and mark below so one row can't wedge the queue.
				slog.Error("failed to send transaction notification", "chatID", chatID, "tx", tx.TxID, "err", err)
			}
			if err := t.store.MarkTransactionNotified(ctx, sqlcgen.MarkTransactionNotifiedParams{
				TgMessageID: msgID,
				TxID:        tx.TxID,
			}); err != nil {
				slog.Error("failed to mark transaction notified", "tx", tx.TxID, "err", err)
				return
			}
			if !sleepCtx(ctx, notifyPause()) {
				return
			}
		}
	}
}

// sendTransactionNotification returns the sent card's message id, or nil when
// the send failed.
func (t *TelegramBot) sendTransactionNotification(ctx context.Context, chatID int64, tx sqlcgen.Transaction, silent bool) (*int64, error) {
	slog.Info("announcing transaction", "chatID", chatID, "tx", tx.TxID, "amt", tx.Amount, "silent", silent)
	msg, err := t.bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:              chatID,
		Text:                formatTransactionMessage(tx),
		ParseMode:           models.ParseModeHTML,
		ReplyMarkup:         t.transactionKeyboardFor(ctx, tx),
		DisableNotification: silent,
	})
	if err != nil || msg == nil {
		return nil, err
	}
	id := int64(msg.ID)
	return &id, nil
}

// transactionKeyboardFor builds a card's keyboard from the row's state,
// including the one-tap merge offer when the row still points at a live
// tip-range candidate. A candidate that has since settled or vanished just
// means no button; the stored pointer is left to age out harmlessly.
func (t *TelegramBot) transactionKeyboardFor(ctx context.Context, tx sqlcgen.Transaction) models.InlineKeyboardMarkup {
	kb := transactionKeyboard(tx.TxID, tx.Excluded == 1)
	if tx.MergeCandidateTxID == nil || tx.Excluded == 1 {
		return kb
	}
	cand, err := t.store.GetTransaction(ctx, *tx.MergeCandidateTxID)
	if err != nil || cand.Pending != 1 {
		return kb
	}
	offer := []models.InlineKeyboardButton{{
		Text:         fmt.Sprintf("🔗 Same as pending %s?", formatDollarsCents(cand.Amount)),
		CallbackData: "merge:" + tx.TxID,
	}}
	kb.InlineKeyboard = append([][]models.InlineKeyboardButton{offer}, kb.InlineKeyboard...)
	return kb
}

// handleMerge executes a user-confirmed tip merge: the settled row the tapped
// card announced is folded onto its pending sibling, whose original card then
// gets refreshed (new amount, settled state) by the stale sweep. The tapped
// card itself is deleted - it is fresh, so within Telegram's delete window -
// or struck through if deletion fails.
func (t *TelegramBot) handleMerge(ctx context.Context, b *bot.Bot, update *models.Update) {
	txID := strings.TrimPrefix(update.CallbackQuery.Data, "merge:")
	settled, err := t.store.GetTransaction(ctx, txID)
	if err != nil || settled.MergeCandidateTxID == nil {
		t.answerCallback(ctx, b, update, "Nothing to merge")
		return
	}
	pending, err := t.store.GetTransaction(ctx, *settled.MergeCandidateTxID)
	if err != nil || pending.Pending != 1 {
		// The candidate settled on its own (or is gone): retract the offer.
		if err := t.store.ClearMergeCandidate(ctx, settled.TxID); err != nil {
			slog.Error("failed to clear merge candidate", "tx", settled.TxID, "err", err)
		}
		t.refreshMessage(ctx, b, update, settled.TxID)
		t.answerCallback(ctx, b, update, "That pending already settled")
		return
	}
	err = t.store.ExecTx(ctx, func(q *sqlcgen.Queries) error {
		// Delete first so the unique plaid id is free to move onto the
		// pending row; a failed merge rolls the delete back.
		if err := q.DeleteTransactionByTxID(ctx, settled.TxID); err != nil {
			return err
		}
		res, err := q.MergeSettledIntoPending(ctx, sqlcgen.MergeSettledIntoPendingParams{
			PostedPlaidID:      settled.PlaidTxID,
			Date:               settled.Date,
			AuthorizedDate:     settled.AuthorizedDate,
			Amount:             settled.Amount,
			Name:               settled.Name,
			MerchantName:       settled.MerchantName,
			CategoryPrimary:    settled.CategoryPrimary,
			CategoryDetailed:   settled.CategoryDetailed,
			CategoryConfidence: settled.CategoryConfidence,
			PaymentChannel:     settled.PaymentChannel,
			LogoUrl:            settled.LogoUrl,
			RawJson:            settled.RawJson,
			PendingTxID:        pending.TxID,
		})
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return fmt.Errorf("merge matched %d rows: %w", n, err)
		}
		return nil
	})
	if err != nil {
		slog.Error("failed to merge transactions", "settled", settled.TxID, "pending", pending.TxID, "err", err)
		t.answerCallback(ctx, b, update, errTryLater)
		return
	}
	slog.Info("merged settled transaction into pending", "settled", settled.TxID, "pending", pending.TxID, "amt", settled.Amount)
	t.recomputeSpend(ctx)
	if msg := update.CallbackQuery.Message.Message; msg != nil {
		_, err := b.DeleteMessage(ctx, &bot.DeleteMessageParams{ChatID: msg.Chat.ID, MessageID: msg.ID})
		if err != nil {
			slog.Warn("failed to delete merged card, striking through instead", "chatID", msg.Chat.ID, "msg", msg.ID, "err", err)
			settled.MergeCandidateTxID = nil
			if err := t.editRemovedTransactionMessage(ctx, msg.Chat.ID, int64(msg.ID), settled, "merged into the card above"); err != nil {
				slog.Warn("failed to strike through merged card", "chatID", msg.Chat.ID, "msg", msg.ID, "err", err)
			}
		}
		// The pending row's card refresh (new amount + reaction) rides the
		// stale sweep.
		t.startDrain(msg.Chat.ID)
	}
	t.answerCallback(ctx, b, update, "Merged")
}

// sweepStaleMessages re-edits announcement cards whose rows changed underneath
// them - a pending that settled via adoption - then marks the settlement with
// a reaction so the change is noticeable without a new message or ping. Rows
// with no recorded card (announcement failed, or the row settled before it was
// ever announced) just get their flag cleared. Edit failures other than rate
// limiting are permanent (card deleted by auto-delete, or too old to edit):
// they log and clear so one dead card can't wedge the sweep. Returns false
// when the context ended and the caller should stop.
func (t *TelegramBot) sweepStaleMessages(ctx context.Context, chatID int64) bool {
	rows, err := t.store.ListStaleMessages(ctx, &chatID)
	if err != nil {
		slog.Error("failed to list stale messages", "chatID", chatID, "err", err)
		return true
	}
	for _, row := range rows {
		tx := row.Transaction
		if tx.TgMessageID != nil {
			// A removed row's card becomes a struck-through tombstone with a
			// thumbs-down; a settled row's card re-renders and gets an okay.
			var err error
			reaction := "👌"
			if tx.RemovedAt != nil {
				reaction = "👎"
				err = t.editRemovedTransactionMessage(ctx, chatID, *tx.TgMessageID, tx, "never settled - dropped by your bank")
			} else {
				err = t.editTransactionMessage(ctx, chatID, *tx.TgMessageID, tx)
			}
			var tooMany *bot.TooManyRequestsError
			if errors.As(err, &tooMany) {
				// Leave the flag set; the next kick retries this card.
				return sleepCtx(ctx, time.Duration(tooMany.RetryAfter)*time.Second)
			}
			if err != nil {
				slog.Warn("failed to refresh transaction card", "chatID", chatID, "tx", tx.TxID, "err", err)
			} else {
				t.react(ctx, chatID, int(*tx.TgMessageID), reaction)
			}
		}
		if err := t.store.ClearMessageStale(ctx, tx.TxID); err != nil {
			slog.Error("failed to clear stale message flag", "tx", tx.TxID, "err", err)
			return true
		}
		if !sleepCtx(ctx, notifyPause()) {
			return false
		}
	}
	return true
}

// editTransactionMessage rewrites an announcement card from the row's current
// state, same render as refreshMessage. Telegram rejects edits that change
// nothing; that means the card is already right, so it counts as success.
func (t *TelegramBot) editTransactionMessage(ctx context.Context, chatID, messageID int64, tx sqlcgen.Transaction) error {
	_, err := t.bot.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID:      chatID,
		MessageID:   int(messageID),
		Text:        formatTransactionMessage(tx),
		ParseMode:   models.ParseModeHTML,
		ReplyMarkup: t.transactionKeyboardFor(ctx, tx),
	})
	if err != nil && strings.Contains(err.Error(), "message is not modified") {
		return nil
	}
	return err
}

// editRemovedTransactionMessage strikes a dead row's card through, appends the
// reason, and drops the buttons - there is nothing left to classify.
func (t *TelegramBot) editRemovedTransactionMessage(ctx context.Context, chatID, messageID int64, tx sqlcgen.Transaction, note string) error {
	_, err := t.bot.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID:    chatID,
		MessageID: int(messageID),
		Text:      "<s>" + formatTransactionBody(tx) + "</s>\n<i>" + note + "</i>",
		ParseMode: models.ParseModeHTML,
	})
	if err != nil && strings.Contains(err.Error(), "message is not modified") {
		return nil
	}
	return err
}

// notifyPause returns the jittered per-message send delay.
func notifyPause() time.Duration {
	return minNotifyPause + rand.N(notifyPauseJitter)
}

// sleepCtx pauses for d, returning false if ctx ends first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
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
				{Text: "📊 Spread", CallbackData: "menu:amort:" + txID},
				{Text: "🚫 Exclude", CallbackData: "exclude:" + txID},
			},
		},
	}
}

// amortizeKeyboard is the second-level menu shown after tapping Spread.
// The period row always shows so a mis-tapped period can be corrected by
// tapping again; Unspread (clear) only appears once the row is spread.
func amortizeKeyboard(txID string, amortized bool) models.InlineKeyboardMarkup {
	bottom := []models.InlineKeyboardButton{
		{Text: "⬅️ Back", CallbackData: "menu:main:" + txID},
	}
	if amortized {
		bottom = append([]models.InlineKeyboardButton{
			{Text: "❌ Unspread", CallbackData: "mortize:" + txID},
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

// sendLinkedMessage confirms a successful account link. The first-ever item
// gets the full onboarding explainer, including the auto-delete tip (bots
// can't set a chat's auto-delete timer, so the user has to).
func (t *TelegramBot) sendLinkedMessage(ctx context.Context, chatID int64, institution string, firstItem bool) {
	inst := html.EscapeString(institution)
	text := fmt.Sprintf("🏦 Linked <b>%s</b>! I'll post its new transactions here as they come in.", inst)
	if firstItem {
		text = fmt.Sprintf(
			"🏦 Thanks for linking <b>%s</b>!\n\n"+
				"I'm collecting your last %d days of transactions now and will place them "+
				"in this chat slowly and silently, so you can classify them at your convenience.\n\n"+
				"Each one gets two buttons:\n"+
				"📊 <b>Spread</b> — spreads a big one-off purchase across weeks or months so it doesn't blow your budget in a single day\n"+
				"🚫 <b>Exclude</b> — leaves it out of your discretionary spend entirely (handy for a rent payment or transfer that slipped through)\n\n"+
				"💡 Tip: set this chat's auto-delete to 1 month (chat menu → Auto-Delete) to keep the clutter down.",
			inst, notifyWindowDays)
	}
	_, err := t.bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:    chatID,
		Text:      text,
		ParseMode: models.ParseModeHTML,
	})
	if err != nil {
		slog.Error("failed to send linked message", "chatID", chatID, "err", err)
	}
}

// announceThreshold is the pending count at which a drain pass introduces
// itself before sending. Day-to-day webhook activity stays under it, so only
// genuine backfills (new account, long downtime) get a header message.
const announceThreshold = 10

// announceBackfill precedes a large drain with a count/ETA message. It is
// sent by the drain worker itself, so it always lands directly above the
// transactions it describes, whichever trigger kicked the drain. Sent
// silently: the backfill it heralds is silent too.
func (t *TelegramBot) announceBackfill(ctx context.Context, chatID int64, cutoff string) {
	count, err := t.store.CountPendingNotifications(ctx, sqlcgen.CountPendingNotificationsParams{
		TgID:   &chatID,
		Cutoff: cutoff,
	})
	if err != nil {
		slog.Error("failed to count pending notifications", "chatID", chatID, "err", err)
		return
	}
	if count < announceThreshold {
		return
	}
	_, err = t.bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID,
		Text: fmt.Sprintf(
			"📥 %d transactions queued from the last %d days — they'll trickle in silently over %s.",
			count, notifyWindowDays, drainETA(count)),
		DisableNotification: true,
	})
	if err != nil {
		slog.Error("failed to send backfill announcement", "chatID", chatID, "err", err)
	}
}

// drainETA renders the expected drain duration for user-facing text.
func drainETA(count int64) string {
	minutes := int((time.Duration(count)*avgNotifyPause + time.Minute/2) / time.Minute)
	if minutes < 2 {
		return "the next couple of minutes"
	}
	return fmt.Sprintf("about %d minutes", minutes)
}

func formatLinkMessage(ttl time.Duration) string {
	return fmt.Sprintf(
		"🔗 <b>Connect Your Bank Account</b>\n\n"+
			"Tap the link below to securely connect your account through Plaid. "+
			"This link is <b>single-use</b> and expires in %s.\n\n"+
			"Once it's linked, I'll pull the last %d days of transactions and start tracking them against your budget.",
		humanDuration(ttl), notifyWindowDays)
}

func formatDashMessage(ttl time.Duration) string {
	return fmt.Sprintf(
		"📊 <b>Your Runway Dashboard</b>\n\n"+
			"Tap the link below to open your dashboard in the browser. "+
			"The link is <b>single-use</b> and expires in %s; once opened, you'll stay signed in for %s.",
		humanDuration(ttl), humanDuration(database.SessionTTL))
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

// formatTransactionMessage renders a transaction's announcement card. An
// excluded row renders struck through with the exclusion noted below it, so a
// glance at the chat shows it no longer counts - no reaction accompanies it,
// since the user flipped the switch themselves.
func formatTransactionMessage(tx sqlcgen.Transaction) string {
	body := formatTransactionBody(tx)
	if tx.Excluded == 1 {
		return "<s>" + body + "</s>\n🚫 <i>excluded from spend</i>"
	}
	return body
}

// formatTransactionBody is the card text without exclusion styling; the
// removed-row tombstone wraps this directly so strikes never nest.
func formatTransactionBody(tx sqlcgen.Transaction) string {
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

	b.WriteString(fmt.Sprintf("%s <b>%s%s</b>  %s\n", emoji, sign, formatDollarsCents(absAmount), html.EscapeString(label)))

	if tx.CategoryPrimary != "" {
		cat := displayCategory(tx.CategoryPrimary)
		if tx.CategoryDetailed != "" {
			cat += " › " + displayCategory(tx.CategoryDetailed)
		}
		b.WriteString(fmt.Sprintf("🏷 %s\n", html.EscapeString(cat)))
	}

	// The authorized (swipe) date is when the money actually left the user's
	// life; the posted date is banking plumbing. Show the one spend counts on.
	b.WriteString(fmt.Sprintf("📅 %s", humanDate(stringOr(tx.AuthorizedDate, tx.Date))))

	if tx.PaymentChannel != "" {
		b.WriteString(fmt.Sprintf("  ·  %s", html.EscapeString(tx.PaymentChannel)))
	}

	if tx.Pending == 1 {
		b.WriteString("\n⏳ <i>pending</i>")
	}

	if tx.AmortEnd != nil {
		b.WriteString(fmt.Sprintf("\n📊 <i>spread until %s</i>", html.EscapeString(humanDate(*tx.AmortEnd))))
	}

	return b.String()
}
