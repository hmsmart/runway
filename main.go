package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hmsmart/runway/database"
	"github.com/hmsmart/runway/domains"
	"github.com/plaid/plaid-go/v43/plaid"
)

//go:embed static/*
var staticFiles embed.FS

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

// retry runs fn with exponential backoff until it succeeds, the context is
// canceled, or the attempt budget (~8s total) is spent. Startup races a
// just-killed predecessor during dev hot-reloads: the old process can briefly
// hold the port, the sqlite lock, or the telegram session.
func retry[T any](ctx context.Context, name string, fn func() (T, error)) (T, error) {
	const attempts = 6
	backoff := 250 * time.Millisecond
	var zero T
	for i := 1; ; i++ {
		v, err := fn()
		if err == nil {
			return v, nil
		}
		if i == attempts {
			return zero, fmt.Errorf("%s: %w", name, err)
		}
		slog.Warn("startup step failed, retrying", "step", name, "attempt", i, "err", err)
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
}

func run(ctx context.Context) error {
	cfg := LoadSettings()

	store, err := retry(ctx, "open database", func() (*database.Store, error) {
		return database.GetStore(ctx, cfg.DBPath, cfg.TokenTTL)
	})
	if err != nil {
		return err
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

	tg, err := retry(ctx, "connect telegram", func() (*TelegramBot, error) {
		return NewTelegramBot(ctx, cfg, store, plaidClient)
	})
	if err != nil {
		return err
	}
	tg.RegisterHandlers()
	go tg.bot.Start(ctx)
	slog.Info("telegram setup")

	//Start HTTP Server
	// Sub into the "static" directory so URLs don't need the "static/" prefix
	subFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("GET /healthz", handleHealthz(store))
	mux.Handle("GET /link", handleLink(plaidClient, cfg, store))
	mux.Handle("POST /link", handleMagicConfirm(store, "/link"))
	mux.Handle("GET /dash", handleDash(store))
	mux.Handle("POST /dash", handleMagicConfirm(store, "/dashboard"))
	mux.Handle("GET /dashboard", requireSession(handleDashboard))
	mux.Handle("POST /exchange-token", handleTokenExchange(plaidClient, store, cfg, tg))
	mux.Handle("POST "+webhookPath(cfg.PlaidWebhookURL), handlePlaidWebhook(plaidClient, store, cfg, tg))
	mux.Handle("GET /profilepic", handleProfilePic(store))
	mux.HandleFunc("GET /login", handleLogin)
	mux.Handle("GET /logout", handleLogout(store))
	//Static pages
	mux.HandleFunc("GET /privacy", handlePrivacy)
	mux.HandleFunc("GET /{$}", handleIndex)
	//assets
	mux.Handle("GET /assets/", handleStatic(http.StripPrefix("/assets/", http.FileServerFS(subFS))))
	// Catch-all — must be last
	mux.HandleFunc("GET /", handleError)

	// Session resolution wraps everything so any page (and the templates'
	// shared Nav) can see who is logged in via the request context.
	srv := &http.Server{Addr: cfg.ListenAddress, Handler: withSessionUser(store, mux)}

	ln, err := retry(ctx, "bind "+cfg.ListenAddress, func() (net.Listener, error) {
		return net.Listen("tcp", cfg.ListenAddress)
	})
	if err != nil {
		return err
	}

	srvErr := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != http.ErrServerClosed {
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
