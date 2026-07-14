package charts

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// TrendDays is the window the bar+line chart shows by default.
const TrendDays = 90

// TrendWindow resolves the chart's ?start=/?end= zoom parameters into a
// concrete date range. Values are YYYY-MM-DD; anything unparsable falls back
// to the default (end = today, start = end minus the default window), a
// reversed pair is swapped, and end never runs past today — there is no data
// beyond it, only dead plot.
func TrendWindow(startS, endS string, today time.Time) (start, end time.Time) {
	end = today
	if t, err := time.Parse(time.DateOnly, endS); err == nil && t.Before(today) {
		end = t
	}
	start = end.AddDate(0, 0, -(TrendDays - 1))
	if t, err := time.Parse(time.DateOnly, startS); err == nil {
		start = t
	}
	if start.After(end) {
		start, end = end, start
	}
	return start, end
}

// Trend renders daily spend as recessive bars with the three EMA rates drawn
// over them as lines — the stock-chart view of the runway series. Bars wear a
// neutral fill so the ordinal blue lines alone carry series identity. The
// caller decides the window (see TrendWindow); the renderer draws whatever
// range it's handed, thinning bars and month labels as the range widens.
//
// With capped true (the default view), the y axis sizes to the typical day —
// the 95th-percentile spend, never below the EMA lines plus headroom — so a
// few spike days can't crush the rates into the baseline. Off-scale bars
// fade out at the plot top; hovering any day (the whole column is the hit
// target, not just the bar) reads out its date and exact spend in the
// top-right corner. capped false restores the fit-everything axis.
func Trend(days []Day, capped bool) string {
	const (
		w, h        = 800, 300
		left        = 52
		right       = 14
		topPad      = 34 // legend row
		bottom      = 26 // x labels
		plotW       = w - left - right
		plotBottomY = h - bottom
		plotH       = plotBottomY - topPad
	)

	maxSpend, maxEMA := 0.0, 0.0
	var nonzero []float64
	for _, d := range days {
		maxSpend = max(maxSpend, d.Spend)
		if d.Spend > 0 {
			nonzero = append(nonzero, d.Spend)
		}
		for _, e := range []*float64{d.E14, d.E28, d.E84} {
			if e != nil {
				maxEMA = max(maxEMA, *e)
			}
		}
	}
	target := max(maxSpend, maxEMA)
	if capped && len(nonzero) > 0 {
		sort.Float64s(nonzero)
		p95 := nonzero[int(0.95*float64(len(nonzero)-1))]
		// Lines always get 30% headroom; the cap never exceeds the real max
		// (an axis taller than the data would just re-add the dead space).
		target = min(max(maxEMA*1.3, p95), target)
	}
	yMax := niceMax(target)
	yOf := func(v float64) float64 { return plotBottomY - v/yMax*plotH }

	var b strings.Builder
	svgOpen(&b, w, h, "daily spend with 14/28/84-day rates", ".bar-stop{stop-color:var(--chart-bar,#cdd3da)}")
	// Off-scale bars wear this fade so they read as continuing past the top.
	b.WriteString(`<defs><linearGradient id="fade" x1="0" y1="0" x2="0" y2="1"><stop offset="0" class="bar-stop" stop-opacity="0"/><stop offset="0.6" class="bar-stop" stop-opacity="1"/></linearGradient></defs>`)

	// Legend: neutral bar swatch plus the three line keys.
	lx := float64(left)
	legendItem := func(swatch, label string) {
		b.WriteString(swatch)
		fmt.Fprintf(&b, `<text x="%s" y="18" font-size="11" class="muted">%s</text>`, f(lx+20), label)
		lx += 20 + float64(len(label))*6.5 + 18
	}
	legendItem(fmt.Sprintf(`<rect x="%s" y="8" width="14" height="10" rx="2" class="bar"/>`, f(lx)), "daily spend")
	for _, s := range emaSlots {
		legendItem(fmt.Sprintf(`<line x1="%s" y1="13" x2="%s" y2="13" stroke="currentColor" stroke-width="2.5" class="%s"/>`, f(lx), f(lx+14), s.class), s.label)
	}

	// Gridlines + y ticks (4 divisions of the nice max).
	for i := 1; i <= 4; i++ {
		v := yMax * float64(i) / 4
		y := yOf(v)
		fmt.Fprintf(&b, `<line x1="%d" y1="%s" x2="%d" y2="%s" class="grid"/>`, left, f(y), w-right, f(y))
		fmt.Fprintf(&b, `<text x="%d" y="%s" font-size="10" class="muted" text-anchor="end">%s</text>`, left-6, f(y+3.5), esc(fmtWhole(v)))
	}
	fmt.Fprintf(&b, `<line x1="%d" y1="%d" x2="%d" y2="%d" class="axis"/>`, left, plotBottomY, w-right, plotBottomY)

	n := len(days)
	if n == 0 {
		fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="12" class="muted" text-anchor="middle">no spending history yet</text>`, w/2, h/2)
		b.WriteString("</svg>")
		return b.String()
	}
	pitch := float64(plotW) / float64(n)
	// The 2px gap yields to keeping bars visible: once the range is so wide
	// that a gapped bar would vanish, bars go contiguous instead.
	barW := pitch - 2
	if barW < 1.2 {
		barW = pitch * 0.85
	}
	xMid := func(i int) float64 { return float64(left) + (float64(i)+0.5)*pitch }

	// Bars, rounded at the data end only, square on the baseline. Days past
	// the capped axis run the full plot height and fade out at the top.
	// Every day — including zero-spend days — is a hover cell: an invisible
	// full-column hit rect (a bar can be a sliver of a pixel when zoomed
	// out) that reveals the day's readout and rings its bar.
	for i, d := range days {
		x := float64(left) + float64(i)*pitch + (pitch-barW)/2
		readout := fmt.Sprintf("%s — %s", dayLabel(d.Date), fmtCents(d.Spend))
		fmt.Fprintf(&b, `<g class="cell" aria-label="%s"><rect x="%s" y="%d" width="%s" height="%d" fill="transparent"/>`,
			esc(readout), f(float64(left)+float64(i)*pitch), topPad, f(pitch), plotBottomY-topPad)
		switch {
		case d.Spend > yMax:
			fmt.Fprintf(&b, `<rect x="%s" y="%d" width="%s" height="%d" fill="url(#fade)" class="mark"/>`,
				f(x), topPad, f(barW), plotBottomY-topPad)
		case d.Spend > 0:
			y := yOf(d.Spend)
			r := min(2.5, (plotBottomY-y)/2, barW/2)
			fmt.Fprintf(&b, `<path class="bar mark" d="M%s %dV%sQ%s %s %s %sH%sQ%s %s %s %sV%dZ"/>`,
				f(x), plotBottomY, f(y+r), f(x), f(y), f(x+r), f(y),
				f(x+barW-r), f(x+barW), f(y), f(x+barW), f(y+r), plotBottomY)
		}
		fmt.Fprintf(&b, `<text x="%d" y="18" font-size="11" class="readout" text-anchor="end">%s</text></g>`,
			w-right, esc(readout))
	}

	// Month boundaries label the x axis. Zoomed out, a month narrows on
	// screen, so labels thin to quarters and then to Januaries rather than
	// colliding.
	monthPx := pitch * 30
	labeledMonth := func(m time.Month) bool {
		switch {
		case monthPx >= 40:
			return true
		case monthPx >= 12:
			return m == time.January || m == time.April || m == time.July || m == time.October
		default:
			return m == time.January
		}
	}
	for i, d := range days {
		t, err := time.Parse(time.DateOnly, d.Date)
		if err != nil {
			continue
		}
		if (t.Day() == 1 || (i == 0 && t.Day() <= 3)) && labeledMonth(t.Month()) {
			fmt.Fprintf(&b, `<text x="%s" y="%d" font-size="10" class="muted" text-anchor="middle">%s</text>`,
				f(xMid(i)), h-8, strings.ToLower(t.Format("Jan 2")))
		}
	}

	// EMA lines, longest horizon drawn first so the freshest sits on top.
	for si := len(emaSlots) - 1; si >= 0; si-- {
		s := emaSlots[si]
		var pts []string
		for i, d := range days {
			v := [3]*float64{d.E14, d.E28, d.E84}[si]
			if v == nil {
				continue
			}
			pts = append(pts, f(xMid(i))+","+f(yOf(*v)))
		}
		if len(pts) < 2 {
			continue
		}
		fmt.Fprintf(&b, `<polyline points="%s" fill="none" stroke="currentColor" stroke-width="2" stroke-linejoin="round" stroke-linecap="round" class="%s"/>`,
			strings.Join(pts, " "), s.class)
	}

	b.WriteString("</svg>")
	return b.String()
}
