package main

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/hmsmart/runway/charts"
	"github.com/hmsmart/runway/database"
	"github.com/hmsmart/runway/database/sqlcgen"
	"github.com/hmsmart/runway/domains"
)

// writeSVG sends a rendered chart. Charts are per-user documents behind the
// session, so shared caches must not hold them; no-cache (not no-store) still
// lets the browser revalidate cheaply on back/forward.
func writeSVG(w http.ResponseWriter, svg string) {
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Write([]byte(svg))
}

// chartDays loads the user's stored daily-spend series between two dates
// (until "" means through the end of the table). The hourly sweep and every
// sync/classification tap keep the table fresh, so chart reads don't trigger
// a recompute of their own.
func chartDays(ctx context.Context, store *database.Store, userID, since, until string) ([]charts.Day, error) {
	rows, err := store.ListDailySpendByUserSince(ctx, sqlcgen.ListDailySpendByUserSinceParams{
		UserID: userID,
		Date:   since,
	})
	if err != nil {
		return nil, fmt.Errorf("list daily spend: %w", err)
	}
	out := make([]charts.Day, 0, len(rows))
	for _, r := range rows {
		if until != "" && r.Date > until {
			break // rows arrive date-ascending
		}
		out = append(out, charts.Day{Date: r.Date, Spend: r.Spend, E14: r.Ema14, E28: r.Ema28, E84: r.Ema84})
	}
	return out, nil
}

// categoryEMAs computes today's per-category smoothed rates straight from
// the transactions table. Every category's EMA folds over the same day range
// (the user's first spend day through today), so the rates are comparable
// and sum to the total series' EMAs. Uncategorized transactions ride under
// the synthetic Other name.
func categoryEMAs(ctx context.Context, store *database.Store, userID string) ([]charts.Category, error) {
	txs, err := store.ListSpendTransactionsByUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list spend transactions: %w", err)
	}
	today := time.Now().Format(time.DateOnly)
	byCat := make(map[string][]sqlcgen.ListSpendTransactionsByUserRow)
	for _, tx := range txs {
		name := tx.CategoryPrimary
		if name == "" {
			name = charts.OtherName
		}
		byCat[name] = append(byCat[name], tx)
	}
	// The shared fold start: earliest countable day across all categories.
	_, first := dailyTotals(txs, today)
	if first == "" {
		return nil, nil
	}
	start, err := time.Parse(time.DateOnly, first)
	if err != nil {
		return nil, fmt.Errorf("parse first spend date %q: %w", first, err)
	}
	end, err := time.Parse(time.DateOnly, today)
	if err != nil {
		return nil, fmt.Errorf("parse today %q: %w", today, err)
	}
	var out []charts.Category
	for name, catTxs := range byCat {
		daily, _ := dailyTotals(catTxs, today)
		var ema emaState
		recent := false
		for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
			ds := d.Format(time.DateOnly)
			ema.step(daily[ds])
			// Recency counts materialized slices, not transaction dates, so a
			// Spread purchase keeps its category live through its window.
			if daily[ds] > 0 && !end.AddDate(0, 0, -14).After(d) {
				recent = true
			}
		}
		out = append(out, charts.Category{Name: name, E14: ema.e14, E28: ema.e28, E84: ema.e84, Recent: recent})
	}
	return out, nil
}

// chartHandler wraps the shared load-then-render shape of every chart
// endpoint: resolve the session user, build the SVG, report failures through
// the styled error page.
func chartHandler(render func(r *http.Request, userID string) (string, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// requireSession guarantees a user is present.
		user := domains.UserFromContext(r.Context())
		svg, err := render(r, user.ID())
		if err != nil {
			httpError(r.Context(), w, clientIP(r), http.StatusInternalServerError, "internal error", "chart", r.URL.Path, "err", err)
			return
		}
		writeSVG(w, svg)
	}
}

func handleChartHeatmap(store *database.Store) http.HandlerFunc {
	return chartHandler(func(r *http.Request, userID string) (string, error) {
		since := time.Now().AddDate(0, 0, -charts.HeatmapWeeks*7).Format(time.DateOnly)
		days, err := chartDays(r.Context(), store, userID, since, "")
		if err != nil {
			return "", err
		}
		return charts.Heatmap(days, time.Now()), nil
	})
}

func handleChartTrend(store *database.Store) http.HandlerFunc {
	return chartHandler(func(r *http.Request, userID string) (string, error) {
		q := r.URL.Query()
		start, end := charts.TrendWindow(q.Get("start"), q.Get("end"), time.Now())
		days, err := chartDays(r.Context(), store, userID,
			start.Format(time.DateOnly), end.Format(time.DateOnly))
		if err != nil {
			return "", err
		}
		return charts.Trend(days, q.Get("scale") != "full"), nil
	})
}

func handleChartCategoryBars(store *database.Store) http.HandlerFunc {
	return chartHandler(func(r *http.Request, userID string) (string, error) {
		cats, err := categoryEMAs(r.Context(), store, userID)
		if err != nil {
			return "", err
		}
		return charts.CategoryBars(cats), nil
	})
}

func handleChartCategoryDonut(store *database.Store) http.HandlerFunc {
	return chartHandler(func(r *http.Request, userID string) (string, error) {
		cats, err := categoryEMAs(r.Context(), store, userID)
		if err != nil {
			return "", err
		}
		return charts.CategoryDonut(cats, charts.ParseView(r.URL.Query().Get("view"))), nil
	})
}

func handleChartCategoryRadar(store *database.Store) http.HandlerFunc {
	return chartHandler(func(r *http.Request, userID string) (string, error) {
		cats, err := categoryEMAs(r.Context(), store, userID)
		if err != nil {
			return "", err
		}
		return charts.CategoryRadar(cats), nil
	})
}

// loadDailies loads the trailing 90 days of the user's stored daily-spend
// series — the shared window every gauge derives its state from.
func loadDailies(ctx context.Context, store *database.Store, userID string) ([]sqlcgen.DailySpend, error) {
	since := time.Now().AddDate(0, 0, -90).Format(time.DateOnly)
	return store.ListDailySpendByUserSince(ctx, sqlcgen.ListDailySpendByUserSinceParams{
		UserID: userID,
		Date:   since,
	})
}

// emaSnapshot is the freshest smoothed state on the stored series.
type emaSnapshot struct {
	E14, E28         float64 // most recent rates
	PrevE14, PrevE28 float64 // one row earlier (equal to latest when absent)
	Target           float64 // burn target: the longest positive horizon available
}

// snapshotEMAs walks the series backwards collecting the latest and previous
// rates per horizon. gauge names the caller in the too-little-history error.
func snapshotEMAs(dailies []sqlcgen.DailySpend, gauge string) (emaSnapshot, error) {
	var s emaSnapshot
	haveE14, haveE28, havePrev14, havePrev28 := false, false, false, false
	for i := len(dailies) - 1; i >= 0; i-- {
		d := dailies[i]
		if d.Ema14 != nil {
			if !haveE14 {
				s.E14, haveE14 = *d.Ema14, true
			} else if !havePrev14 {
				s.PrevE14, havePrev14 = *d.Ema14, true
			}
		}
		if d.Ema28 != nil {
			if !haveE28 {
				s.E28, haveE28 = *d.Ema28, true
			} else if !havePrev28 {
				s.PrevE28, havePrev28 = *d.Ema28, true
			}
		}
		if s.Target == 0 {
			switch {
			case d.Ema84 != nil && *d.Ema84 > 0:
				s.Target = *d.Ema84
			case d.Ema28 != nil && *d.Ema28 > 0:
				s.Target = *d.Ema28
			case d.Ema14 != nil && *d.Ema14 > 0:
				s.Target = *d.Ema14
			}
		}
		if haveE14 && haveE28 && havePrev14 && havePrev28 && s.Target > 0 {
			break
		}
	}
	if !haveE14 || !haveE28 {
		return s, fmt.Errorf("insufficient %s ema history", gauge)
	}
	if !havePrev14 {
		s.PrevE14 = s.E14
	}
	if !havePrev28 {
		s.PrevE28 = s.E28
	}
	return s, nil
}

// monthToDateSpend sums the series' spend for now's calendar month.
func monthToDateSpend(dailies []sqlcgen.DailySpend, now time.Time) float64 {
	monthPrefix := now.Format("2006-01")
	total := 0.0
	for _, d := range dailies {
		if strings.HasPrefix(d.Date, monthPrefix) {
			total += d.Spend
		}
	}
	return total
}

// daysInMonth is the calendar length of now's month.
func daysInMonth(now time.Time) int {
	return time.Date(now.Year(), now.Month()+1, 0, 0, 0, 0, 0, now.Location()).Day()
}

// hasDepository reports whether any tracked account holds actual cash (the
// same non-credit rule cashOnHand counts). Without one, runway can't be
// computed and the fuel instruments must read INOP rather than a fake zero.
func hasDepository(accounts []sqlcgen.Account) bool {
	for _, a := range accounts {
		if a.Type == nil || *a.Type != "credit" {
			return true
		}
	}
	return false
}

// syncFresh reports whether any tracked account synced recently enough for
// the panel to be trusted. Balances refresh on every transaction sync, so a
// day and a half of silence means the pipeline is stuck, not that spending
// stopped.
func syncFresh(accounts []sqlcgen.Account) bool {
	for _, a := range accounts {
		if a.LastSyncedAt != nil && time.Since(*a.LastSyncedAt) < 36*time.Hour {
			return true
		}
	}
	return false
}

func handleGaugeVSI(store *database.Store) http.HandlerFunc {
	return chartHandler(func(r *http.Request, userID string) (string, error) {
		dailies, err := loadDailies(r.Context(), store, userID)
		if err != nil {
			return "", err
		}
		accounts, err := store.ListTrackedAccountsByUser(r.Context(), userID)
		if err != nil {
			return "", err
		}
		if !hasDepository(accounts) {
			return charts.VSI(nil, 0), nil
		}

		// VSI uses available depository cash only (not net of credit owed),
		// so it reflects direct runway-day sensitivity to burn-rate changes.
		cash, _ := cashOnHand(accounts)
		if cash <= 0 {
			return charts.VSI(nil, 0), nil
		}

		today := time.Now().Format(time.DateOnly)
		runwayDays := make([]float64, 0, len(dailies))
		for _, v := range dailies {
			// Use only complete days; today's partial spend can swing EMA and
			// produce an exaggerated intraday runway delta.
			if v.Date < today && v.Ema14 != nil {
				// Very small EMA14 values explode runway-days math and can
				// dominate the gauge scale with unrealistic historical spikes.
				if *v.Ema14 < 1 {
					continue
				}
				days := runwayFuelDays(cash, *v.Ema14)
				if !math.IsInf(days, 0) && !math.IsNaN(days) {
					runwayDays = append(runwayDays, days)
				}
			}
		}

		// Keep the gauge adaptive to current behavior by using a recent window
		// for scale fitting rather than the full historical tail.
		const maxRunwaySamples = 45
		if len(runwayDays) > maxRunwaySamples {
			runwayDays = runwayDays[len(runwayDays)-maxRunwaySamples:]
		}
		if len(runwayDays) < 2 {
			return "", fmt.Errorf("insufficient runway-day history")
		}

		deltas := make([]float64, 0, len(runwayDays)-1)
		for i := 1; i < len(runwayDays); i++ {
			// Positive means runway days are increasing (improving).
			deltas = append(deltas, runwayDays[i]-runwayDays[i-1])
		}
		if len(deltas) == 0 {
			return "", fmt.Errorf("insufficient delta history")
		}
		current := deltas[len(deltas)-1]
		return charts.VSI(deltas, current), nil
	})
}

func handleGaugeADI(store *database.Store) http.HandlerFunc {
	return chartHandler(func(r *http.Request, userID string) (string, error) {
		dailies, err := loadDailies(r.Context(), store, userID)
		if err != nil {
			return "", err
		}
		userRow, err := store.GetUserByID(r.Context(), userID)
		if err != nil {
			return "", err
		}
		s, err := snapshotEMAs(dailies, "adi")
		if err != nil {
			return "", err
		}
		now := time.Now()
		burnMTD := monthToDateSpend(dailies, now)

		targetDaily := s.Target
		hasBudget := userRow.DiscretionaryMonthly != nil && *userRow.DiscretionaryMonthly > 0
		daysLeft := max(daysInMonth(now)-now.Day()+1, 1)
		var devDays, devDollars float64
		if hasBudget {
			dailyAllowance := *userRow.DiscretionaryMonthly / float64(daysInMonth(now))
			plannedCumulative := dailyAllowance * float64(now.Day())
			devDollars = burnMTD - plannedCumulative
			if dailyAllowance > 0 {
				devDays = devDollars / dailyAllowance
			}
			if targetDaily <= 0 {
				targetDaily = dailyAllowance
			}
		}

		accounts, err := store.ListTrackedAccountsByUser(r.Context(), userID)
		if err != nil {
			return "", err
		}
		cash, owed := cashOnHand(accounts)
		net := cash - owed
		var fuelDays float64
		if net > 0 && s.E14 > 0 {
			fuelDays = runwayFuelDays(net, s.E14)
		}

		state := charts.ComputeADI(targetDaily, s.E14, s.E28, burnMTD, hasBudget, devDays, devDollars, fuelDays, daysLeft)
		return charts.ADI(state), nil
	})
}

func handleGaugeCAS(store *database.Store) http.HandlerFunc {
	return chartHandler(func(r *http.Request, userID string) (string, error) {
		dailies, err := loadDailies(r.Context(), store, userID)
		if err != nil {
			return "", err
		}
		userRow, err := store.GetUserByID(r.Context(), userID)
		if err != nil {
			return "", err
		}
		s, err := snapshotEMAs(dailies, "cas")
		if err != nil {
			return "", err
		}
		accounts, err := store.ListTrackedAccountsByUser(r.Context(), userID)
		if err != nil {
			return "", err
		}

		now := time.Now()
		burnMTD := monthToDateSpend(dailies, now)
		db := computeDailyBudget(r.Context(), store, userID)
		targetDaily := db.Allowance
		daysLeft := 0
		remainingBudget := 0.0
		fuelWarn := false
		fuelLow := false
		consumed := 0.0

		if db.HasBudget {
			monthly := *userRow.DiscretionaryMonthly
			daysLeft = max(daysInMonth(now)-now.Day()+1, 1)
			remainingBudget = max(monthly-burnMTD, 0)
			consumed = burnMTD / monthly
			if daysLeft > 0 && remainingBudget/float64(daysLeft) < s.E14*0.5 {
				fuelLow = true
			}
			if remainingBudget <= 0 {
				fuelLow = true
			}
			cash, owed := cashOnHand(accounts)
			net := cash - owed
			if net > 0 && s.E14 > 0 && runwayFuelDays(net, s.E14) < float64(daysLeft) {
				fuelWarn = true
			}
		} else if s.Target > 0 {
			targetDaily = s.Target
		}

		annState := charts.ComputeAnnunciator(
			targetDaily, s.E14, s.E28,
			s.PrevE14, s.PrevE28,
			daysLeft, remainingBudget,
			syncFresh(accounts), fuelWarn, fuelLow,
		)

		cash, owed := cashOnHand(accounts)
		net := cash - owed
		hasFuel := false
		var days14, days28 float64
		if net > 0 && s.E14 > 0 {
			hasFuel = true
			days14 = runwayFuelDays(net, s.E14)
			days28 = runwayFuelDays(net, s.E28)
		}

		state := charts.CASPanelState{
			Annunciator: annState,
			Target:      formatDollars(math.Round(db.Allowance)),
			Commit:      formatDollarsCents(db.Committed),
			SpentToday:  formatDollarsCents(db.Spent),
			TargetVal:   db.Allowance,
			CommitVal:   db.Committed,
			SpentTodVal: db.Spent,
			Reduction:   db.Correction,
			HasBudget:   db.HasBudget,
			Consumed:    consumed,
			HasFuel:     hasFuel,
			Days14:      days14,
			Days28:      days28,
		}
		return charts.CASPanelSVG(state), nil
	})
}


// runwayFuelDays converts cash and a burn rate to days of runway for the fuel
// gauge: zero when the cash is gone, infinite when nothing is burning.
func runwayFuelDays(net, rate float64) float64 {
	if net <= 0 {
		return 0
	}
	if rate <= 0 {
		return math.Inf(1)
	}
	return net / rate
}

func handleGaugeFuel(store *database.Store) http.HandlerFunc {
	return chartHandler(func(r *http.Request, userID string) (string, error) {
		dailies, err := loadDailies(r.Context(), store, userID)
		if err != nil {
			return "", err
		}
		s, err := snapshotEMAs(dailies, "fuel")
		if err != nil {
			return "", err
		}
		accounts, err := store.ListTrackedAccountsByUser(r.Context(), userID)
		if err != nil {
			return "", err
		}
		if !hasDepository(accounts) {
			return charts.FuelINOP(), nil
		}
		cash, owed := cashOnHand(accounts)
		net := cash - owed

		allDailies, err := store.ListDailySpendByUserSince(r.Context(), sqlcgen.ListDailySpendByUserSinceParams{
			UserID: userID,
			Date:   "",
		})
		if err != nil {
			return "", err
		}
		totalBurn := 0.0
		for _, d := range allDailies {
			totalBurn += d.Spend
		}

		return charts.Fuel(runwayFuelDays(net, s.E14), runwayFuelDays(net, s.E28), totalBurn), nil
	})
}


func handleGaugeEGT(store *database.Store) http.HandlerFunc {
	return chartHandler(func(r *http.Request, userID string) (string, error) {
		cats, err := categoryEMAs(r.Context(), store, userID)
		if err != nil {
			return "", err
		}
		return charts.EGTPanel(cats), nil
	})
}

func handleGaugeFMSBills(store *database.Store) http.HandlerFunc {
	return chartHandler(func(r *http.Request, userID string) (string, error) {
		rows, err := store.ListActiveBillsByUser(r.Context(), userID)
		if err != nil {
			return "", fmt.Errorf("list bills: %w", err)
		}

		now := time.Now()
		var bills []charts.Bill
		for _, row := range rows {
			bills = append(bills, charts.Bill{
				Name:    row.Name,
				Amount:  row.Amount,
				DaysOut: daysUntilDayOfMonth(now, int(row.DayOfMonth)),
			})
		}

		accounts, err := store.ListTrackedAccountsByUser(r.Context(), userID)
		if err != nil {
			return "", fmt.Errorf("list accounts: %w", err)
		}
		cash, owed := cashOnHand(accounts)
		net := math.Max(cash-owed, 0)

		userRow, err := store.GetUserByID(r.Context(), userID)
		if err != nil {
			return "", fmt.Errorf("get user: %w", err)
		}
		payAmount := 0.0
		payDay := 0
		if userRow.PayAmount != nil && userRow.PayDay != nil {
			payAmount = *userRow.PayAmount
			payDay = daysUntilDayOfMonth(now, int(*userRow.PayDay))
		}

		state := charts.FMSBillsState{
			Bills:      bills,
			CashOnHand: net,
			PayAmount:  payAmount,
			PayDay:     payDay,
		}
		return charts.FMSBillsSVG(state), nil
	})
}

func daysUntilDayOfMonth(now time.Time, dayOfMonth int) int {
	today := now.Day()
	if dayOfMonth >= today {
		return dayOfMonth - today
	}
	nextMonth := time.Date(now.Year(), now.Month()+1, dayOfMonth, 0, 0, 0, 0, now.Location())
	return int(nextMonth.Sub(now).Hours()/24) + 1
}
