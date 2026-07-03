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

	db, err := sql.NewDatabase(ctx, cfg.DBPath)
	if err != nil {
		return fmt.Errorf("initialize database: %w", err)
	}
	defer func() { _ = db.Close() }()
	dbBinded := sqlcgen.New(db)
	items, err := dbBinded.GetAllItems(ctx)
	if err != nil {
		return fmt.Errorf("get all items: %w", err)
	}
	slog.Info("Items", "count", len(items))
	for _, item := range items {
		slog.Info("Item", "item_id", item.ItemID, "institution_name", item.InstitutionName.String)
		myItem, err := dbBinded.GetItemByID(ctx, item.ItemID)
		if err != nil {
			return fmt.Errorf("get item by id: %w", err)
		}
		slog.Info("Item Details", "item_id", myItem.ItemID, "institution_name", myItem.InstitutionName.String, "status", myItem.Status, "created_at", myItem.CreatedAt, "last_synced_at", myItem.LastSyncedAt, "access_token", myItem.AccessToken, "cursor", myItem.Cursor)
		myToken, err := internal.DecryptColumnSecret(myItem.AccessToken, myItem.ItemID, cfg.DBCryptKey)
		if err != nil {
			return fmt.Errorf("decrypt access token: %w", err)
		}
		slog.Info("Decrypted Access Token", "item_id", myItem.ItemID, "access_token", myToken)
		encToken, err := internal.EncryptColumnSecret(myToken, myItem.ItemID, cfg.DBCryptKey)
		if err != nil {
			return fmt.Errorf("encrypt access token: %w", err)
		}
		slog.Info("Re-Encrypted Access Token", "item_id", myItem.ItemID, "access_token", encToken)

	}
	return nil
}
