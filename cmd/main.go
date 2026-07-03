package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/hmsmart/runway"
	sql "github.com/hmsmart/runway/database"
	"github.com/hmsmart/runway/database/sqlcgen"
	"github.com/hmsmart/runway/internal"
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
	cfg := runway.LoadSettings()

	// Initialize database connection
	dbConn, err := sql.NewDatabase(ctx, cfg.DBPath)
	if err != nil {
		return fmt.Errorf("initialize database: %w", err)
	}
	defer func() { _ = dbConn.Close() }()
	db := sqlcgen.New(dbConn)
	slog.Info("Database connection established", "db_path", cfg.DBPath)
	//Initialize Plaid Connection
	plaidConfiguration := plaid.NewConfiguration()
	plaidConfiguration.AddDefaultHeader("PLAID-CLIENT-ID", cfg.PlaidClientID)
	plaidConfiguration.AddDefaultHeader("PLAID-SECRET", cfg.PlaidSecret)
	plaidConfiguration.UseEnvironment(cfg.PlaidEnv)

	plaidClient := plaid.NewAPIClient(plaidConfiguration)
	slog.Info("Opened Plaid API")
	//Get All Account Balances
	itemsCtx, itemsCancel := context.WithTimeout(ctx, cfg.PlaidTimeout)
	items, err := db.GetAllItems(itemsCtx)
	itemsCancel()
	if err != nil {
		return fmt.Errorf("get all items: %w", err)
	}
	slog.Info("Retrieved all accounts", "count", len(items))
	for _, item := range items {
		slog.Info("Retrieving account balances for item", "item_id", item.ItemID, "institution_name", item.InstitutionName)
		accessToken, err := internal.DecryptColumnSecret(item.AccessToken, item.ItemID, cfg.DBCryptKey)
		if err != nil {
			slog.Error("Failed to decrypt access token", "error", err)
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, cfg.PlaidTimeout)
		balancesGetRequest := plaid.NewAccountsBalanceGetRequest(accessToken)
		balancesResp, _, err := plaidClient.PlaidApi.AccountsBalanceGet(callCtx).AccountsBalanceGetRequest(
			*balancesGetRequest,
		).Execute()
		cancel()
		if err != nil {
			slog.Error("Failed to retrieve account balances", "error", err)
			continue
		}
		slog.Info("Account balances retrieved successfully", "item_id", item.ItemID, "balances", balancesResp.GetAccounts())
	}
	return nil
}
