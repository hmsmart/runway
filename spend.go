package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/hmsmart/runway/database"
	"github.com/hmsmart/runway/database/sqlcgen"
)

// EMA smoothing factors, alpha = 2/(N+1) for an N-day EMA.
const (
	alpha14 = 2.0 / 15.0
	alpha28 = 2.0 / 29.0
)

// recomputeDailySpend rebuilds a user's entire daily_spend series from the
// transactions table. A full rebuild (rather than incremental updates) is
// deliberate: Plaid modifies and removes past transactions, and Spread and
// Exclude taps rewrite history, any of which invalidates every EMA from that
// day forward. The data is small enough that recomputing is cheaper than
// being clever, and the result is idempotent no matter which trigger fired.
func recomputeDailySpend(ctx context.Context, store *database.Store, userID string) error {
	txs, err := store.ListSpendTransactionsByUser(ctx, userID)
	if err != nil {
		return fmt.Errorf("list spend transactions: %w", err)
	}
	today := time.Now().Format(time.DateOnly)
	daily, first := dailyTotals(txs, today)
	return store.ExecTx(ctx, func(q *sqlcgen.Queries) error {
		// Replace wholesale: rows are pure derived data.
		if err := q.DeleteDailySpendByUser(ctx, userID); err != nil {
			return fmt.Errorf("clear daily spend: %w", err)
		}
		if first == "" {
			return nil
		}
		start, err := time.Parse(time.DateOnly, first)
		if err != nil {
			return fmt.Errorf("parse first spend date %q: %w", first, err)
		}
		end, err := time.Parse(time.DateOnly, today)
		if err != nil {
			return fmt.Errorf("parse today %q: %w", today, err)
		}
		// Every day in the range gets a row, including zero-spend days —
		// the EMAs must decay through quiet stretches.
		var ema14, ema28 float64
		seeded := false
		for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
			ds := d.Format(time.DateOnly)
			spend := daily[ds]
			if seeded {
				ema14 = alpha14*spend + (1-alpha14)*ema14
				ema28 = alpha28*spend + (1-alpha28)*ema28
			} else {
				ema14, ema28 = spend, spend
				seeded = true
			}
			err := q.InsertDailySpend(ctx, sqlcgen.InsertDailySpendParams{
				Date:   ds,
				UserID: userID,
				Spend:  spend,
				Ema14:  &ema14,
				Ema28:  &ema28,
			})
			if err != nil {
				return fmt.Errorf("insert daily spend %s: %w", ds, err)
			}
		}
		return nil
	})
}

// dailyTotals folds countable transactions into per-day spend through today,
// returning the totals and the earliest day with a contribution ("" if none).
// A spread transaction contributes amount/N to each day of its window, where
// N is the days from date (inclusive) to amort_end (exclusive) — matching
// SetAmortEnd's "+7 days" producing exactly 7 daily slices. Days after today
// are left for future recomputes to materialize.
func dailyTotals(txs []sqlcgen.ListSpendTransactionsByUserRow, today string) (map[string]float64, string) {
	daily := make(map[string]float64)
	first := ""
	note := func(day string, amt float64) {
		daily[day] += amt
		if first == "" || day < first {
			first = day
		}
	}
	for _, tx := range txs {
		// Future-dated rows (rare Plaid quirk) count when their day arrives.
		if tx.Date > today {
			continue
		}
		if tx.AmortEnd == nil || *tx.AmortEnd <= tx.Date {
			note(tx.Date, tx.Amount)
			continue
		}
		start, err1 := time.Parse(time.DateOnly, tx.Date)
		end, err2 := time.Parse(time.DateOnly, *tx.AmortEnd)
		if err1 != nil || err2 != nil {
			// A malformed date can't prorate; count it unspread rather than
			// silently dropping spend.
			note(tx.Date, tx.Amount)
			continue
		}
		days := int(end.Sub(start).Hours() / 24)
		perDay := tx.Amount / float64(days)
		for d := start; d.Before(end); d = d.AddDate(0, 0, 1) {
			ds := d.Format(time.DateOnly)
			if ds > today {
				break
			}
			note(ds, perDay)
		}
	}
	return daily, first
}

// handleRunway reports today's spend, the smoothed daily rates, and how long
// the user's cash lasts at those rates — the number the app is named for.
func (t *TelegramBot) handleRunway(ctx context.Context, b *bot.Bot, update *models.Update) {
	user := UserFromContext(ctx)
	chatID := update.Message.Chat.ID
	slog.Info("called runway", "chatID", chatID)
	// Recompute first: the stored series may be up to an hour stale, and
	// this is the moment the user is actually looking at the numbers.
	if err := recomputeDailySpend(ctx, t.store, user.ID()); err != nil {
		slog.Error("failed to recompute daily spend", "user", user.ID(), "err", err)
		t.sendText(ctx, b, chatID, errTryLater)
		return
	}
	today := time.Now().Format(time.DateOnly)
	day, err := t.store.GetDailySpendDay(ctx, sqlcgen.GetDailySpendDayParams{
		UserID: user.ID(),
		Date:   today,
	})
	if errors.Is(err, sql.ErrNoRows) {
		t.sendText(ctx, b, chatID,
			"No spending history yet — connect a bank account with <code>/link</code> and I'll start tracking your runway.")
		return
	} else if err != nil {
		slog.Error("failed to load daily spend", "user", user.ID(), "err", err)
		t.sendText(ctx, b, chatID, errTryLater)
		return
	}
	accounts, err := t.store.ListTrackedAccountsByUser(ctx, user.ID())
	if err != nil {
		slog.Error("failed to list tracked accounts", "user", user.ID(), "err", err)
		t.sendText(ctx, b, chatID, errTryLater)
		return
	}
	t.sendText(ctx, b, chatID, formatRunwayMessage(day, accounts))
}

// formatRunwayMessage renders the /runway report as an aligned two-column
// ledger inside a <pre> block. Cash on hand is depository balances (available
// preferred, since that's what's actually spendable) minus credit-card
// balances owed: cards are spend that hasn't left checking yet, so ignoring
// them would overstate the runway.
func formatRunwayMessage(day sqlcgen.DailySpend, accounts []sqlcgen.Account) string {
	var cash, owed float64
	for _, a := range accounts {
		if a.Type != nil && *a.Type == "credit" {
			if a.BalanceCurrent != nil {
				owed += *a.BalanceCurrent
			}
			continue
		}
		if a.BalanceAvailable != nil {
			cash += *a.BalanceAvailable
		} else if a.BalanceCurrent != nil {
			cash += *a.BalanceCurrent
		}
	}
	net := cash - owed
	netStr := formatDollarsCents(net)
	if net < 0 {
		netStr = "-" + formatDollarsCents(-net)
	}

	// A tail (trend marker) hangs off the value column so it never disturbs
	// the alignment; an empty label renders as a group separator.
	type row struct{ label, value, tail string }
	rows := []row{
		{"Spend today", formatDollarsCents(day.Spend), ""},
		{"14-day rate", formatEMA(day.Ema14), trendIndicator(day.Spend, day.Ema14)},
		{"28-day rate", formatEMA(day.Ema28), trendIndicator(day.Spend, day.Ema28)},
		{},
	}
	if owed > 0 {
		rows = append(rows,
			row{"Cash", formatDollarsCents(cash), ""},
			row{"Cards", "-" + formatDollarsCents(owed), ""},
		)
	}
	rows = append(rows,
		row{"On hand", netStr, ""},
		row{},
		row{"Runway @ 14d", runwayDays(net, day.Ema14), ""},
		row{"Runway @ 28d", runwayDays(net, day.Ema28), ""},
	)

	// Pad by rune count, not bytes: values can hold non-ASCII ("∞", "—").
	labelW, valueW := 0, 0
	for _, r := range rows {
		labelW = max(labelW, utf8.RuneCountInString(r.label))
		valueW = max(valueW, utf8.RuneCountInString(r.value))
	}
	var sb strings.Builder
	sb.WriteString("🛫 <b>Runway</b>\n<pre>")
	for _, r := range rows {
		if r.label == "" {
			sb.WriteString("\n")
			continue
		}
		sb.WriteString(r.label)
		sb.WriteString(strings.Repeat(" ", labelW-utf8.RuneCountInString(r.label)+2))
		sb.WriteString(strings.Repeat(" ", valueW-utf8.RuneCountInString(r.value)))
		sb.WriteString(r.value)
		sb.WriteString(r.tail)
		sb.WriteString("\n")
	}
	sb.WriteString("</pre>")
	return sb.String()
}

// trendIndicator marks how today's spend sits against a smoothed rate:
// red when today is outspending it, green when under it. Exactly on the
// rate counts as under — you haven't outspent it yet.
func trendIndicator(spend float64, ema *float64) string {
	if ema == nil {
		return ""
	}
	if spend > *ema {
		return "  🔴"
	}
	return "  🟢"
}

// formatEMA renders a smoothed rate, or a placeholder before one exists.
func formatEMA(v *float64) string {
	if v == nil {
		return "—"
	}
	return formatDollarsCents(*v)
}

// runwayDays renders how long cash lasts at a daily rate. Days are floored:
// runway you can't finish isn't runway.
func runwayDays(cash float64, ema *float64) string {
	if cash <= 0 {
		return "0 days"
	}
	if ema == nil || *ema <= 0 {
		return "∞"
	}
	d := int(cash / *ema)
	if d == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", d)
}

// recomputeAllDailySpend refreshes the series for every active user. It backs
// the hourly sweep, which exists so the series rolls over at midnight (today's
// zero-spend row, EMA decay) even when no webhook or button tap triggers a
// recompute.
func recomputeAllDailySpend(ctx context.Context, store *database.Store) {
	users, err := store.ListActiveUserIDs(ctx)
	if err != nil {
		slog.Error("failed to list users for daily spend sweep", "err", err)
		return
	}
	for _, id := range users {
		if err := recomputeDailySpend(ctx, store, id); err != nil {
			slog.Error("failed to recompute daily spend", "user", id, "err", err)
		}
	}
}
