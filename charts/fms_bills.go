package charts

import (
	"fmt"
	"math"
	"strings"
)

// Bill is one upcoming obligation on the FMS route.
type Bill struct {
	Name    string
	Amount  float64
	DaysOut int
}

// FMSBillsState holds everything the FMS bills panel needs.
type FMSBillsState struct {
	Bills      []Bill
	CashOnHand float64

	// Income projection (optional — zero values disable).
	PayAmount float64
	PayDay    int // day-of-month offset from today, 0 = no income
}

// fmsEvent is one waypoint on the route: a bill (outflow) or paycheck (inflow).
type fmsEvent struct {
	Name     string
	Amount   float64
	DaysOut  int
	IsIncome bool
	Balance  float64
	Status   string // "green", "amber", "red"
}

// FMSBillsSVG renders the FMS-style bills panel as a 200×400 SVG.
// Bills appear as diamond waypoints on a route; an optional paycheck
// appears as a cyan upward triangle. Color encodes cumulative
// affordability: green (comfortable), amber (tight), red (short).
func FMSBillsSVG(s FMSBillsState) string {
	const (
		W, H     = 200, 400
		headerH  = 52
		footerH  = 42
		colHeadH = 14
		cyan     = "#00d4ff"
	)

	hasIncome := s.PayDay > 0 && s.PayAmount > 0

	// Build event list: bills + optional income.
	events := make([]fmsEvent, 0, len(s.Bills)+1)
	for _, b := range s.Bills {
		events = append(events, fmsEvent{
			Name: b.Name, Amount: b.Amount, DaysOut: b.DaysOut,
		})
	}
	if hasIncome {
		events = append(events, fmsEvent{
			Name: "PAYCK", Amount: s.PayAmount, DaysOut: s.PayDay, IsIncome: true,
		})
	}
	// Sort by day, income before bills on the same day.
	sortEvents(events)

	if len(events) == 0 {
		return fmsEmpty(W, H)
	}

	// Walk the route computing running balance.
	balance := s.CashOnHand
	for i := range events {
		if events[i].IsIncome {
			balance += events[i].Amount
		} else {
			balance -= events[i].Amount
		}
		ratio := balance / s.CashOnHand
		switch {
		case ratio >= 0.3:
			events[i].Status = "green"
		case balance > 0:
			events[i].Status = "amber"
		default:
			events[i].Status = "red"
		}
		events[i].Balance = balance
	}

	// Compute total bills (excluding income).
	totalBills := 0.0
	for _, b := range s.Bills {
		totalBills += b.Amount
	}

	// Row height: fit all events into available space, clamped 22–30.
	listArea := H - headerH - footerH - colHeadH
	rowH := listArea / len(events)
	if rowH > 30 {
		rowH = 30
	}
	if rowH < 22 {
		rowH = 22
	}
	visibleCount := listArea / rowH
	if visibleCount > len(events) {
		visibleCount = len(events)
	}
	hiddenCount := len(events) - visibleCount

	var b strings.Builder
	svgOpen(&b, W, H, "FMS bills — upcoming obligations route", `
.fms-panel{fill:#0f1318;stroke:var(--chart-axis,#c6ccd4);stroke-width:1.5;rx:10}
.fms-title{font-size:7px;letter-spacing:2px}
.fms-col{font-size:6px;letter-spacing:1px}
.fms-name{font-size:9px;font-weight:700}
.fms-amt{font-size:8.5px}
.fms-bal{font-size:8.5px;font-weight:700}
.fms-lbl{fill:#ffb000;font-size:7px;font-weight:700;letter-spacing:1px}
.fms-val{font-size:9px;font-weight:700}
.fms-divider{stroke:#262b33;stroke-width:1}
.fms-row-div{stroke:#1a1e25;stroke-width:0.5}
.fms-bar-bg{fill:#14181e}
.fms-connector{stroke:#1a2a1a;stroke-width:1}
.fms-connector-income{stroke:#0a2a3a;stroke-width:1}
`)

	b.WriteString(`<g>`)
	fmt.Fprintf(&b, `<rect x="0" y="0" width="%d" height="%d" rx="10" class="fms-panel"/>`, W, H)

	// Header.
	modeLabel := "RTE BILLS · ACTIVE"
	if hasIncome {
		modeLabel = "RTE BILLS · WITH INCOME"
	}
	fmt.Fprintf(&b, `<text x="100" y="46" text-anchor="middle" class="fms-title" style="fill:var(--chart-muted,#8891a0)">%s</text>`, modeLabel)

	// Column headers.
	hy := headerH + 4
	fmt.Fprintf(&b, `<text x="14" y="%d" class="fms-col" style="fill:#5a6270">WPT</text>`, hy)
	fmt.Fprintf(&b, `<text x="100" y="%d" text-anchor="middle" class="fms-col" style="fill:#5a6270">AMT</text>`, hy)
	fmt.Fprintf(&b, `<text x="140" y="%d" class="fms-col" style="fill:#5a6270">ETA</text>`, hy)
	fmt.Fprintf(&b, `<text x="188" y="%d" text-anchor="end" class="fms-col" style="fill:#5a6270">BAL</text>`, hy)
	fmt.Fprintf(&b, `<line x1="12" y1="%d" x2="188" y2="%d" class="fms-divider"/>`, hy+4, hy+4)

	// Rows.
	listTop := hy + 8
	for i := 0; i < visibleCount; i++ {
		ev := events[i]
		y := listTop + i*rowH
		color := fmsColor(ev)

		// Alternating row background.
		if i%2 == 0 {
			fmt.Fprintf(&b, `<rect x="10" y="%d" width="180" height="%d" fill="rgba(255,255,255,0.012)" rx="2"/>`, y, rowH)
		}

		// Route connector.
		if i > 0 {
			cls := "fms-connector"
			if ev.IsIncome {
				cls = "fms-connector-income"
			}
			fmt.Fprintf(&b, `<line x1="10" y1="%d" x2="10" y2="%d" class="%s"/>`, y, y-rowH+min(18, rowH-4), cls)
		}

		// Waypoint marker.
		if ev.IsIncome {
			fmt.Fprintf(&b, `<polygon points="10,%d 6.5,%d 13.5,%d" fill="%s"/>`, y+10, y+4, y+4, cyan)
		} else {
			fmt.Fprintf(&b, `<polygon points="10,%d 13.5,%d 10,%d 6.5,%d" fill="%s"/>`,
				y+8-4, y+8, y+8+4, y+8, color)
		}

		// Name.
		fmt.Fprintf(&b, `<text x="20" y="%d" class="fms-name" style="fill:%s">%s</text>`, y+11, color, esc(ev.Name))

		// Amount.
		prefix := ""
		if ev.IsIncome {
			prefix = "+"
		}
		amtFill := "var(--chart-ink,#d0d5dc)"
		if ev.IsIncome {
			amtFill = cyan
		}
		fmt.Fprintf(&b, `<text x="100" y="%d" text-anchor="middle" class="fms-amt" style="fill:%s">%s%s</text>`,
			y+11, amtFill, prefix, fmsAmt(ev.Amount))

		// ETA.
		fmt.Fprintf(&b, `<text x="140" y="%d" class="fms-amt" style="fill:var(--chart-muted,#8891a0)">%dD</text>`, y+11, ev.DaysOut)

		// Balance.
		balStr := fmsAmt(ev.Balance)
		if ev.Balance < 0 {
			balStr = "-" + fmsAmt(-ev.Balance)
		}
		fmt.Fprintf(&b, `<text x="188" y="%d" text-anchor="end" class="fms-bal" style="fill:%s">%s</text>`,
			y+11, color, balStr)

		// Fuel bar (only if rows tall enough).
		if rowH >= 26 {
			barY := y + 17
			barW := 172.0
			maxBal := s.CashOnHand
			if hasIncome {
				maxBal += s.PayAmount
			}
			pct := clamp(ev.Balance/maxBal, 0, 1)
			fmt.Fprintf(&b, `<rect x="14" y="%d" width="%s" height="2.5" rx="1.25" class="fms-bar-bg"/>`, barY, f(barW))
			fmt.Fprintf(&b, `<rect x="14" y="%d" width="%s" height="2.5" rx="1.25" fill="%s" opacity="0.45"/>`,
				barY, f(barW*pct), color)
		}

		// Row divider.
		fmt.Fprintf(&b, `<line x1="12" y1="%d" x2="188" y2="%d" class="fms-row-div"/>`, y+rowH-1, y+rowH-1)
	}

	// Overflow indicator.
	if hiddenCount > 0 {
		oy := listTop + visibleCount*rowH + 2
		fmt.Fprintf(&b, `<text x="100" y="%d" text-anchor="middle" font-size="7" style="fill:var(--chart-muted,#8891a0)">+%d MORE</text>`, oy+8, hiddenCount)
	}

	// Footer.
	footY := H - footerH
	fmt.Fprintf(&b, `<line x1="12" y1="%d" x2="188" y2="%d" class="fms-divider"/>`, footY, footY)
	fmt.Fprintf(&b, `<text x="14" y="%d" class="fms-lbl">FUEL</text>`, footY+14)
	fmt.Fprintf(&b, `<text x="50" y="%d" class="fms-val" style="fill:%s">%s</text>`,
		footY+14, gaugeGreen, fmsAmt(s.CashOnHand))
	fmt.Fprintf(&b, `<text x="110" y="%d" class="fms-lbl">REQD</text>`, footY+14)
	reqColor := "var(--chart-ink,#d0d5dc)"
	if totalBills > s.CashOnHand {
		reqColor = gaugeRed
	}
	fmt.Fprintf(&b, `<text x="188" y="%d" text-anchor="end" class="fms-val" style="fill:%s">%s</text>`,
		footY+14, reqColor, fmsAmt(totalBills))

	// End balance / income info.
	endBal := events[len(events)-1].Balance
	endColor := gaugeGreen
	if endBal < 0 {
		endColor = gaugeRed
	}
	if hasIncome {
		fmt.Fprintf(&b, `<text x="14" y="%d" style="fill:%s;font-size:7px;font-weight:700;letter-spacing:1px">REFUEL</text>`, footY+28, cyan)
		fmt.Fprintf(&b, `<text x="60" y="%d" style="fill:%s;font-size:7.5px">+%s @%dD</text>`,
			footY+28, cyan, fmsAmt(s.PayAmount), s.PayDay)
		endStr := fmsAmt(endBal)
		if endBal < 0 {
			endStr = "-" + fmsAmt(-endBal)
		}
		fmt.Fprintf(&b, `<text x="188" y="%d" text-anchor="end" class="fms-val" style="fill:%s">%s</text>`,
			footY+28, endColor, endStr)
	} else {
		fmt.Fprintf(&b, `<text x="14" y="%d" style="fill:var(--chart-muted,#8891a0);font-size:7px;letter-spacing:1px">END BAL</text>`, footY+28)
		endStr := fmsAmt(endBal)
		if endBal < 0 {
			endStr = "-" + fmsAmt(-endBal)
		}
		fmt.Fprintf(&b, `<text x="188" y="%d" text-anchor="end" class="fms-val" style="fill:%s">%s</text>`,
			footY+28, endColor, endStr)
	}

	// Status bar.
	statusY := footY + 34
	barW2 := 172.0
	consumed := math.Min(totalBills/s.CashOnHand, 1.0)
	barCol := gaugeGreen
	if totalBills > s.CashOnHand {
		barCol = gaugeRed
	} else if totalBills > s.CashOnHand*0.75 {
		barCol = gaugeAmber
	}
	fmt.Fprintf(&b, `<rect x="14" y="%d" width="%s" height="3" rx="1.5" class="fms-bar-bg"/>`, statusY, f(barW2))
	fmt.Fprintf(&b, `<rect x="14" y="%d" width="%s" height="3" rx="1.5" fill="%s" opacity="0.6"/>`,
		statusY, f(barW2*consumed), barCol)

	b.WriteString(`</g></svg>`)
	return b.String()
}

// fmsEmpty renders a placeholder panel when no bills are configured.
func fmsEmpty(w, h int) string {
	var b strings.Builder
	svgOpen(&b, w, h, "FMS bills — no bills configured", `
.fms-panel{fill:#0f1318;stroke:var(--chart-axis,#c6ccd4);stroke-width:1.5;rx:10}
.fms-title{font-size:7px;letter-spacing:2px}
`)
	b.WriteString(`<g>`)
	fmt.Fprintf(&b, `<rect x="0" y="0" width="%d" height="%d" rx="10" class="fms-panel"/>`, w, h)
	fmt.Fprintf(&b, `<text x="100" y="46" text-anchor="middle" class="fms-title" style="fill:var(--chart-muted,#8891a0)">RTE BILLS · INACTIVE</text>`)
	fmt.Fprintf(&b, `<text x="100" y="200" text-anchor="middle" font-size="9" style="fill:#333a44">NO BILLS CONFIGURED</text>`)
	fmt.Fprintf(&b, `<text x="100" y="218" text-anchor="middle" font-size="7" style="fill:#333a44">use /bill add or tap ♻ on a transaction</text>`)
	b.WriteString(`</g></svg>`)
	return b.String()
}

// fmsAmt formats a dollar value for the panel: "$1.9k" or "$142".
func fmsAmt(v float64) string {
	v = math.Abs(v)
	if v >= 1000 {
		return fmt.Sprintf("$%.1fk", v/1000)
	}
	return fmt.Sprintf("$%.0f", v)
}

// fmsColor returns the fill color for an event based on its status.
func fmsColor(ev fmsEvent) string {
	if ev.IsIncome {
		return "#00d4ff"
	}
	switch ev.Status {
	case "red":
		return gaugeRed
	case "amber":
		return gaugeAmber
	default:
		return "#4ade80"
	}
}

// sortEvents sorts by DaysOut ascending, income before bills on the same day.
func sortEvents(events []fmsEvent) {
	for i := 1; i < len(events); i++ {
		for j := i; j > 0; j-- {
			a, b := events[j-1], events[j]
			swap := false
			if a.DaysOut > b.DaysOut {
				swap = true
			} else if a.DaysOut == b.DaysOut && !a.IsIncome && b.IsIncome {
				swap = true
			}
			if swap {
				events[j-1], events[j] = events[j], events[j-1]
			}
		}
	}
}
