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

	// Initialize database connection
	dbConn, err := database.NewDatabase(ctx, cfg.DBPath)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	slog.Info("connected to database", "path", cfg.DBPath)
	defer func() { _ = dbConn.Close() }()
	db := sqlcgen.New(dbConn)

	//Connect to Telegram

	tg, err := NewTelegramBot(cfg.TGBotKey, cfg.TGChatId)
	if err != nil {
		return fmt.Errorf("starting telegram: %w", err)
	}
	tg.RegisterHandlers(db)
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
	mux.Handle("GET /healthz", handleHealthz(dbConn))
	mux.Handle("GET /link", handleLink(plaidClient, ctx, *cfg))
	mux.Handle("POST /exchange-token", handleTokenExchange(plaidClient, ctx, *cfg, db))

	srv := &http.Server{Addr: cfg.ListenAddress, Handler: mux}

	srvErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			srvErr <- err
		}
	}()
	slog.Info("runway is up", "addr", cfg.ListenAddress)

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
