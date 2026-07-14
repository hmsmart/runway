package main

import (
	"context"
	"fmt"
	"net/http"
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
