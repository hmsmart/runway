package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hmsmart/runway/database"
	"github.com/hmsmart/runway/database/sqlcgen"
)

// seedAccessToken marks items created by the seed tool instead of a Plaid
// link. Anything that would talk to Plaid for an item (startup sync, unlink)
// checks for it and skips the API call.
const seedAccessToken = "seed"

// runSeed implements the `runway seed` subcommand: it loads a CSV of real
// spending into the database as a fudged Plaid item/account/transactions, so
// the EMA math can be checked against genuine data before Plaid production
// access exists. Rows use deterministic IDs, so re-running the same file
// updates in place rather than duplicating.
//
// CSV template: a header of date,amount,description; dates as YYYY-MM-DD or
// M/D/YYYY; positive amounts are money spent (Plaid's sign convention).
func runSeed(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	file := fs.String("file", "", "CSV file with header date,amount,description (required)")
	tgID := fs.Int64("tg", 0, "telegram chat id of the owning user (default: the sole active user)")
	name := fs.String("name", "", "account name shown in /accounts (default: file basename)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		fs.Usage()
		return errors.New("--file is required")
	}
	if *name == "" {
		*name = strings.TrimSuffix(filepath.Base(*file), filepath.Ext(*file))
	}

	rows, err := parseSeedCSV(*file)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return errors.New("no transactions in file")
	}

	cfg := LoadSettings()
	store, err := database.GetStore(ctx, cfg.DBPath, cfg.TokenTTL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer store.Close()

	user, err := resolveSeedUser(ctx, store, *tgID)
	if err != nil {
		return err
	}

	itemID := "seed:" + user
	accountID := "seed:" + shortHash(user+"/"+*name)
	now := time.Now()
	err = store.ExecTx(ctx, func(q *sqlcgen.Queries) error {
		if _, err := q.GetItemByID(ctx, itemID); errors.Is(err, sql.ErrNoRows) {
			err = q.CreateItem(ctx, sqlcgen.CreateItemParams{
				ItemID:          itemID,
				UserID:          user,
				AccessToken:     seedAccessToken,
				InstitutionName: ptr("Imported"),
				Status:          "active",
			})
			if err != nil {
				return fmt.Errorf("create seed item: %w", err)
			}
		} else if err != nil {
			return fmt.Errorf("load seed item: %w", err)
		}
		err := q.UpsertAccount(ctx, sqlcgen.UpsertAccountParams{
			AccountID:    accountID,
			ItemID:       itemID,
			Name:         *name,
			Tracked:      1,
			LastSyncedAt: &now,
		})
		if err != nil {
			return fmt.Errorf("upsert seed account: %w", err)
		}
		for i, r := range rows {
			txid, err := uuid.NewV7()
			if err != nil {
				return fmt.Errorf("generate tx uuid: %w", err)
			}
			// The Plaid ID is a content hash: re-seeding the same file hits
			// ON CONFLICT(plaid_tx_id) and updates instead of duplicating.
			plaidID := "seed:" + shortHash(fmt.Sprintf("%s|%d|%s|%f|%s", accountID, i, r.date, r.amount, r.description))
			_, err = q.UpsertTransaction(ctx, sqlcgen.UpsertTransactionParams{
				TxID:             txid.String(),
				PlaidTxID:        plaidID,
				AccountID:        accountID,
				Date:             r.date,
				Amount:           r.amount,
				Name:             r.description,
				CategoryPrimary:  "",
				CategoryDetailed: "",
				PaymentChannel:   "other",
				Pending:          0,
				// Pre-notified: seed data must never flood the chat.
				Notified: 1,
			})
			if err != nil {
				return fmt.Errorf("upsert row %d: %w", i+2, err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	if err := recomputeDailySpend(ctx, store, user); err != nil {
		return fmt.Errorf("recompute daily spend: %w", err)
	}

	first, last, total := rows[0].date, rows[0].date, 0.0
	for _, r := range rows {
		first, last = min(first, r.date), max(last, r.date)
		total += r.amount
	}
	fmt.Printf("Seeded %d transactions (%s to %s, net $%.2f) into account %q for user %s.\n",
		len(rows), first, last, total, *name, user)
	fmt.Println("Daily spend series recomputed — send /runway to see it.")
	return nil
}

// resolveSeedUser picks the target user: an explicit --tg id, or the sole
// active user when there's no ambiguity.
func resolveSeedUser(ctx context.Context, store *database.Store, tgID int64) (string, error) {
	if tgID != 0 {
		u, err := store.GetUserByTelegram(ctx, &tgID)
		if err != nil {
			return "", fmt.Errorf("no active user with telegram id %d: %w", tgID, err)
		}
		return u.ID, nil
	}
	users, err := store.ListActiveUserIDs(ctx)
	if err != nil {
		return "", fmt.Errorf("list users: %w", err)
	}
	if len(users) != 1 {
		return "", fmt.Errorf("%d active users; pass --tg to pick one", len(users))
	}
	return users[0], nil
}

type seedRow struct {
	date        string
	amount      float64
	description string
}

// parseSeedCSV reads the seed template: header date,amount,description, then
// one transaction per row. The whole file parses or the whole file fails —
// a half-imported seed would silently skew the EMAs it exists to validate.
func parseSeedCSV(path string) ([]seedRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	reader := csv.NewReader(f)
	reader.TrimLeadingSpace = true
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parse csv: %w", err)
	}
	if len(records) == 0 {
		return nil, errors.New("empty file")
	}
	header := records[0]
	if len(header) > 0 {
		header[0] = strings.TrimPrefix(header[0], "\ufeff") // Excel BOM
	}
	if len(header) != 3 ||
		!strings.EqualFold(strings.TrimSpace(header[0]), "date") ||
		!strings.EqualFold(strings.TrimSpace(header[1]), "amount") ||
		!strings.EqualFold(strings.TrimSpace(header[2]), "description") {
		return nil, fmt.Errorf("header must be date,amount,description; got %q", strings.Join(header, ","))
	}
	rows := make([]seedRow, 0, len(records)-1)
	for i, rec := range records[1:] {
		line := i + 2
		if len(rec) != 3 {
			return nil, fmt.Errorf("line %d: expected 3 fields, got %d", line, len(rec))
		}
		date, err := parseSeedDate(strings.TrimSpace(rec[0]))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		amtStr := strings.NewReplacer("$", "", ",", "", " ", "").Replace(rec[1])
		amount, err := strconv.ParseFloat(amtStr, 64)
		if err != nil {
			return nil, fmt.Errorf("line %d: bad amount %q", line, rec[1])
		}
		desc := strings.TrimSpace(rec[2])
		if desc == "" {
			desc = "Imported"
		}
		rows = append(rows, seedRow{date: date, amount: amount, description: desc})
	}
	return rows, nil
}

// parseSeedDate accepts YYYY-MM-DD or M/D/YYYY (padded or not) and returns
// the storage format, YYYY-MM-DD.
func parseSeedDate(s string) (string, error) {
	for _, layout := range []string{time.DateOnly, "1/2/2006"} {
		if d, err := time.Parse(layout, s); err == nil {
			return d.Format(time.DateOnly), nil
		}
	}
	return "", fmt.Errorf("bad date %q (want YYYY-MM-DD or M/D/YYYY)", s)
}

// shortHash gives a compact stable id component for seeded rows.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

func ptr[T any](v T) *T { return &v }
