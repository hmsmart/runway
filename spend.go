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
	"github.com/hmsmart/runway/domains"
)

// EMA smoothing factors, alpha = 2/(N+1) for an N-day EMA.
const (
	alpha14 = 2.0 / 15.0
	alpha28 = 2.0 / 29.0
	alpha84 = 2.0 / 85.0
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
		var ema14, ema28, ema84 float64
		seeded := false
		for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
			ds := d.Format(time.DateOnly)
			spend := daily[ds]
			if seeded {
				ema14 = alpha14*spend + (1-alpha14)*ema14
				ema28 = alpha28*spend + (1-alpha28)*ema28
				ema84 = alpha84*spend + (1-alpha84)*ema84
			} else {
				ema14, ema28, ema84 = spend, spend, spend
				seeded = true
			}
			err := q.InsertDailySpend(ctx, sqlcgen.InsertDailySpendParams{
				Date:   ds,
				UserID: userID,
				Spend:  spend,
				Ema14:  &ema14,
				Ema28:  &ema28,
				Ema84:  &ema84,
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

// errNoSpendHistory means a runway report can't be built because the user has
// no daily-spend rows yet (no linked account, or no countable transactions).
var errNoSpendHistory = errors.New("no spend history")

// buildRunwayReport recomputes the user's series and renders the runway
// report: yesterday's spend, the smoothed daily rates, and how long the cash
// lasts at those rates — the number the app is named for. Yesterday is the
// anchor because it's the most recent complete day; today's partial total
// would read as a spending dip every morning. Shared by the /runway command
// and the scheduled daily report.
func (t *TelegramBot) buildRunwayReport(ctx context.Context, userID string) (string, error) {
	// Recompute first: the stored series may be up to an hour stale, and
	// this is the moment the user is actually looking at the numbers.
	if err := recomputeDailySpend(ctx, t.store, userID); err != nil {
		return "", fmt.Errorf("recompute daily spend: %w", err)
	}
	now := time.Now()
	spendLabel := "Yesterday"
	day, err := t.store.GetDailySpendDay(ctx, sqlcgen.GetDailySpendDayParams{
		UserID: userID,
		Date:   now.AddDate(0, 0, -1).Format(time.DateOnly),
	})
	if errors.Is(err, sql.ErrNoRows) {
		// The series starts at the first spend day, so a user whose first
		// transaction landed today has no yesterday row yet. Show the
		// partial day rather than claiming there's no history.
		spendLabel = "Today"
		day, err = t.store.GetDailySpendDay(ctx, sqlcgen.GetDailySpendDayParams{
			UserID: userID,
			Date:   now.Format(time.DateOnly),
		})
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", errNoSpendHistory
	} else if err != nil {
		return "", fmt.Errorf("load daily spend: %w", err)
	}
	accounts, err := t.store.ListTrackedAccountsByUser(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("list tracked accounts: %w", err)
	}
	u, err := t.store.GetUserByID(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("load user: %w", err)
	}
	txs, err := t.store.ListSpendTransactionsByUser(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("list spend transactions: %w", err)
	}
	committed := todaysCommitments(txs, now.Format(time.DateOnly))
	// Today's row always exists once there's any history (the recompute
	// materializes every day through today), but tolerate its absence.
	var todaySpend float64
	if todayRow, err := t.store.GetDailySpendDay(ctx, sqlcgen.GetDailySpendDayParams{
		UserID: userID,
		Date:   now.Format(time.DateOnly),
	}); err == nil {
		todaySpend = todayRow.Spend
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("load today's spend: %w", err)
	}
	return formatRunwayMessage(day, accounts, u.DiscretionaryMonthly, committed, todaySpend, spendLabel, now), nil
}

// todaysCommitments sums the slices of spread transactions whose amort window
// covers today — spend already promised to today by earlier Spread taps, as
// opposed to choices still open today. Window semantics match dailyTotals:
// date inclusive, amort_end exclusive. Malformed windows are skipped here
// (dailyTotals counts them unspread, which makes them regular spend, not a
// commitment).
func todaysCommitments(txs []sqlcgen.ListSpendTransactionsByUserRow, today string) float64 {
	var total float64
	for _, tx := range txs {
		if tx.AmortEnd == nil || *tx.AmortEnd <= tx.Date {
			continue
		}
		if tx.Date > today || *tx.AmortEnd <= today {
			continue
		}
		start, err1 := time.Parse(time.DateOnly, tx.Date)
		end, err2 := time.Parse(time.DateOnly, *tx.AmortEnd)
		if err1 != nil || err2 != nil {
			continue
		}
		days := int(end.Sub(start).Hours() / 24)
		total += tx.Amount / float64(days)
	}
	return total
}

func (t *TelegramBot) handleRunway(ctx context.Context, b *bot.Bot, update *models.Update) {
	user := domains.UserFromContext(ctx)
	chatID := update.Message.Chat.ID
	slog.Info("called runway", "chatID", chatID)
	msg, err := t.buildRunwayReport(ctx, user.ID())
	if errors.Is(err, errNoSpendHistory) {
		t.sendText(ctx, b, chatID,
			"No spending history yet — connect a bank account with <code>/link</code> and I'll start tracking your runway.")
		return
	} else if err != nil {
		slog.Error("failed to build runway report", "user", user.ID(), "err", err)
		t.sendText(ctx, b, chatID, errTryLater)
		return
	}
	t.sendText(ctx, b, chatID, msg)
}

// formatRunwayMessage renders the /runway report as an aligned two-column
// ledger inside a <pre> block. It opens with today's plan when a budget is
// set: the day's allowance (monthly budget spread evenly over the month's
// days), what spread transactions have already committed against today, what's
// been swiped so far today, and what remains. Cash on hand is depository
// balances (available
// preferred, since that's what's actually spendable) minus credit-card
// balances owed: cards are spend that hasn't left checking yet, so ignoring
// them would overstate the runway.
func formatRunwayMessage(day sqlcgen.DailySpend, accounts []sqlcgen.Account, budget *float64, committed, todaySpend float64, spendLabel string, now time.Time) string {
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

	// A tail (trend marker or landing date) hangs off the value column so it
	// never disturbs the alignment; an empty label renders as a group
	// separator. Each trend compares a rate to the next-longer horizon —
	// spend vs 14d, 14d vs 28d, 28d vs 84d — so red means spending is
	// running hotter than its longer-term baseline.
	type row struct{ label, value, tail string }
	var rows []row
	if budget != nil {
		// Days in the whole month, not days remaining: the allowance is a
		// steady daily figure, not a pace-corrected one.
		daysInMonth := time.Date(now.Year(), now.Month()+1, 0, 0, 0, 0, 0, now.Location()).Day()
		allowance := *budget / float64(daysInMonth)
		// Today's series row already folds in the amort slices, so subtracting
		// committed leaves just what was actually swiped today. Clamp: float
		// drift could otherwise show a tiny negative.
		spent := max(todaySpend-committed, 0)
		available := allowance - committed - spent
		rows = append(rows, row{"Allowance", formatDollarsCents(allowance), ""})
		if committed > 0 {
			rows = append(rows, row{"Commitments", "-" + formatDollarsCents(committed), ""})
		}
		if spent > 0 {
			rows = append(rows, row{"Spent today", "-" + formatDollarsCents(spent), ""})
		}
		availableStr := formatDollarsCents(available)
		if available < 0 {
			availableStr = "-" + formatDollarsCents(-available)
		}
		rows = append(rows,
			row{"Available", availableStr, ""},
			row{},
		)
	}
	rows = append(rows,
		row{spendLabel, formatDollarsCents(day.Spend), trendIndicator(day.Spend, day.Ema14)},
		row{"14-day rate", formatEMA(day.Ema14), emaTrend(day.Ema14, day.Ema28)},
		row{"28-day rate", formatEMA(day.Ema28), emaTrend(day.Ema28, day.Ema84)},
		row{},
	)
	if owed > 0 {
		rows = append(rows,
			row{"Cash", formatDollarsCents(cash), ""},
			row{"Cards", "-" + formatDollarsCents(owed), ""},
		)
	}
	rows = append(rows,
		row{"On hand", netStr, ""},
		row{},
		row{"Runway @ 14d", runwayDays(net, day.Ema14), runwayEnd(now, net, day.Ema14)},
		row{"Runway @ 28d", runwayDays(net, day.Ema28), runwayEnd(now, net, day.Ema28)},
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

// trendIndicator marks how a spend figure sits against a longer-horizon
// baseline: red when it's outspending the baseline, green when under it.
// Exactly on the baseline counts as under — you haven't outspent it yet.
func trendIndicator(spend float64, baseline *float64) string {
	if baseline == nil {
		return ""
	}
	if spend > *baseline {
		return "  🔴"
	}
	return "  🟢"
}

// emaTrend is trendIndicator for comparing two smoothed rates, where the
// shorter horizon may itself be missing.
func emaTrend(ema, baseline *float64) string {
	if ema == nil {
		return ""
	}
	return trendIndicator(*ema, baseline)
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

// runwayEnd renders the calendar day the cash runs out at a daily rate, as a
// tail on the runway row. Empty when there's no finite runway to land — the
// day count already says "0 days" or "∞". The year is spelled out only when
// the landing day isn't in the current year, where "Jan 02" alone would be
// ambiguous.
func runwayEnd(now time.Time, cash float64, ema *float64) string {
	if cash <= 0 || ema == nil || *ema <= 0 {
		return ""
	}
	end := now.AddDate(0, 0, int(cash / *ema))
	if end.Year() != now.Year() {
		return "  → " + end.Format("Jan 02, 2006")
	}
	return "  → " + end.Format("Jan 02")
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
