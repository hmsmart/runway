package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hmsmart/runway/database"
	"github.com/hmsmart/runway/domains"
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

	store, err := database.GetStore(ctx, cfg.DBPath, cfg.TokenTTL)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}

	slog.Info("connected to database", "path", cfg.DBPath)
	defer func() { _ = store.Close() }()

	// Initialize Plaid

	plaidConfiguration := plaid.NewConfiguration()
	plaidConfiguration.AddDefaultHeader("PLAID-CLIENT-ID", cfg.PlaidClientID)
	plaidConfiguration.AddDefaultHeader("PLAID-SECRET", cfg.PlaidSecret)
	plaidConfiguration.UseEnvironment(cfg.PlaidEnv)
	plaidClient := plaid.NewAPIClient(plaidConfiguration)
	slog.Info("Plaid initialized")

	//Connect to Telegram

	tg, err := NewTelegramBot(ctx, cfg, store, plaidClient)
	if err != nil {
		return fmt.Errorf("starting telegram: %w", err)
	}
	tg.RegisterHandlers()
	go tg.bot.Start(ctx)
	slog.Info("telegram setup")

	//Start HTTP Server
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", handleHealthz(store))
	mux.Handle("GET /link", handleLink(plaidClient, cfg, store))
	mux.Handle("POST /exchange-token", handleTokenExchange(plaidClient, store, cfg, tg))
	mux.Handle("POST "+webhookPath(cfg.PlaidWebhookURL), handlePlaidWebhook(plaidClient, store, cfg, tg))

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
		// Crash recovery first: drain anything queued before the last
		// shutdown, even for items whose sync below fails.
		chats, err := store.ListChatsWithPendingNotifications(ctx)
		if err != nil {
			slog.Error("failed to list chats with pending notifications", "err", err)
		}
		for _, chat := range chats {
			if chat != nil {
				tg.startDrain(*chat)
			}
		}
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
			if err := syncItem(ctx, item.ItemID, accessToken, item.Cursor, plaidClient, store, cfg); err != nil {
				slog.Error("startup sync failed", "item", item.ItemID, "err", err)
			}
			usql, err := store.GetUserByID(ctx, item.UserID)
			if err != nil {
				slog.Error("failed to load user for item", "item", item.ItemID, "user", item.UserID, "err", err)
				continue
			}
			if u := domains.NewUser(usql); u != nil {
				tg.startDrain(u.TelegramID())
			}
		}
	}()

	// Hourly daily-spend sweep. Syncs and classification taps recompute the
	// series themselves; this exists so it still rolls over at midnight
	// (today's zero-spend row, EMA decay) on days with no other activity.
	// Hourly rather than a midnight timer keeps it timezone-shift-proof and
	// costs nothing: the recompute is idempotent over tiny data.
	go func() {
		tick := time.NewTicker(time.Hour)
		defer tick.Stop()
		for {
			recomputeAllDailySpend(ctx, store)
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()

	// Scheduled daily reports. The 30s tick only bounds how late a report can
	// be past its slot; dueness itself is state in the users table, so a slot
	// missed to downtime sends on the first tick after startup.
	go func() {
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			tg.sendDueReports(ctx)
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
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
