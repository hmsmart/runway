package main

import (
	"context"
	"fmt"
	"html"
	"log"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/hmsmart/runway/database"
	"github.com/hmsmart/runway/database/sqlcgen"
	"github.com/plaid/plaid-go/v43/plaid"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))

	// CTRL+C for SIGINT call and kill for SIGTERM
	// These are common OS signals to signal app to shutdown gracefully
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	cfg := LoadSettings()

	store, err := database.GetStore(ctx, cfg.DBPath)
	if err != nil {
		return fmt.Errorf("failed to open databse: %w", err)
	}

	slog.Info("connected to database", "path", cfg.DBPath)
	defer func() { _ = store.Close() }()

	//Connect to Telegram

	tg, err := NewTelegramBot(cfg.TGBotKey, cfg.TGChatId)
	//Setup TG callback
	notify := func(ctx context.Context, tx sqlcgen.UpsertTransactionParams) {
		slog.Info("New Transaction", "id", tx.TxID, "PlTx", tx.PlaidTxID, "amt", tx.Amount)
		if tx.Amount < 0 {
			return
		}
		tg.bot.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:      cfg.TGChatId,
			Text:        formatTransactionMessage(tx),
			ParseMode:   models.ParseModeHTML,
			ReplyMarkup: transactionKeyboard(tx.TxID),
		})
	}
	if err != nil {
		return fmt.Errorf("starting telegram: %w", err)
	}
	tg.RegisterHandlers(store)
	go tg.bot.Start(ctx)
	slog.Info("telegram setup")

	// Initialize Plaid

	plaidConfiguration := plaid.NewConfiguration()
	plaidConfiguration.AddDefaultHeader("PLAID-CLIENT-ID", cfg.PlaidClientID)
	plaidConfiguration.AddDefaultHeader("PLAID-SECRET", cfg.PlaidSecret)
	plaidConfiguration.UseEnvironment(cfg.PlaidEnv)
	plaidClient := plaid.NewAPIClient(plaidConfiguration)
	slog.Info("Plaid initialized")

	//Start HTTP Server
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", handleHealthz(store))
	mux.Handle("GET /link", handleLink(plaidClient, cfg))
	mux.Handle("POST /exchange-token", handleTokenExchange(plaidClient, store, cfg, notify))

	srv := &http.Server{Addr: cfg.ListenAddress, Handler: mux}

	srvErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			srvErr <- err
		}
	}()
	slog.Info("runway is up", "addr", cfg.ListenAddress)

	slog.Info("program init synchronizing transactions")
	go func() {
		items, err := store.GetAllItems(ctx)
		if err != nil {
			slog.Error("failed to load items during startup synchronization", "err", err)
			return
		}
		for _, item := range items {
			accessToken, err := DecryptColumnSecret(item.AccessToken, item.ItemID, cfg.DBCryptKey)
			if err != nil {
				slog.Error("failed to decrypt access token", "item", item.ItemID, "err", err)
				continue
			}
			if err := syncItem(ctx, item.ItemID, accessToken, NullStringToPtr(item.Cursor), plaidClient, store, cfg, notify); err != nil {
				slog.Error("startup sync failed", "item", item.ItemID, "err", err)
			}
		}
	}()

	select {
	case err := <-srvErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}
	// graceful shutdown
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func transactionKeyboard(txID string) models.InlineKeyboardMarkup {
	return models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{
				{Text: "📊 Amortize", CallbackData: "noop"},
			},
			{
				{Text: "1 Week", CallbackData: "amort:1w:" + txID},
				{Text: "1 Month", CallbackData: "amort:1m:" + txID},
				{Text: "1 Year", CallbackData: "amort:1y:" + txID},
			},
			{
				{Text: "🚫 Exclude", CallbackData: "exclude:" + txID},
			},
		},
	}
}

func formatTransactionMessage(tx sqlcgen.UpsertTransactionParams) string {
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
		cat := categoryDisplay[tx.CategoryPrimary.String]
		if cat == "" {
			cat = displayCategory(tx.CategoryPrimary.String)
		}
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

	return b.String()
}
