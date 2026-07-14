package charts

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// HeatmapWeeks is how much history the calendar heatmap shows. 26 columns at
// a col-8 width keeps the cells finger-sized; a full GitHub year would halve
// them.
const HeatmapWeeks = 26

// Heatmap renders the GitHub-style spending calendar: one column per week,
// one row per weekday, each day's cell stepped through the sequential heat
// ramp by how hot that day's spend ran. Levels are quartiles of the window's
// nonzero days, so the scale adapts to the user rather than to a fixed
// dollar figure. Hovering a cell rings it and prints the day's date and
// spend in the bottom-left readout — pure CSS, so it works wherever the SVG
// document does.
func Heatmap(days []Day, today time.Time) string {
	const (
		w, h    = 800, 264
		top     = 26  // month labels
		left    = 34  // weekday labels
		pitch   = 29  // cell + gap
		cell    = 25  //
		legendY = 246 //
	)

	spend := make(map[string]float64, len(days))
	for _, d := range days {
		spend[d.Date] = d.Spend
	}

	// The grid ends on today's week (Sunday-started) and reaches back a fixed
	// number of columns; trailing cells after today render as future blanks.
	weekStart := today.AddDate(0, 0, -int(today.Weekday()))
	gridStart := weekStart.AddDate(0, 0, -7*(HeatmapWeeks-1))
	todayStr := today.Format(time.DateOnly)

	// Quartile thresholds over the window's nonzero days.
	var nonzero []float64
	for d := gridStart; !d.After(today); d = d.AddDate(0, 0, 1) {
		if v := spend[d.Format(time.DateOnly)]; v > 0 {
			nonzero = append(nonzero, v)
		}
	}
	sort.Float64s(nonzero)
	quart := func(q float64) float64 {
		if len(nonzero) == 0 {
			return 0
		}
		i := int(q * float64(len(nonzero)-1))
		return nonzero[i]
	}
	t1, t2, t3 := quart(0.25), quart(0.5), quart(0.75)
	level := func(v float64) int {
		switch {
		case v <= 0:
			return 0
		case v <= t1:
			return 1
		case v <= t2:
			return 2
		case v <= t3:
			return 3
		default:
			return 4
		}
	}

	var b strings.Builder
	svgOpen(&b, w, h, "daily spending heatmap", heatCSS())

	// Weekday gutter: alternate rows only, GitHub-style.
	for _, wd := range []struct {
		row  int
		name string
	}{{1, "mon"}, {3, "wed"}, {5, "fri"}} {
		y := top + wd.row*pitch + cell/2 + 4
		fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="10" class="muted">%s</text>`, 2, y, wd.name)
	}

	prevMonth := time.Month(0)
	for col := 0; col < HeatmapWeeks; col++ {
		ws := gridStart.AddDate(0, 0, 7*col)
		if ws.Month() != prevMonth {
			fmt.Fprintf(&b, `<text x="%d" y="16" font-size="10" class="muted">%s</text>`,
				left+col*pitch, strings.ToLower(ws.Format("Jan")))
			prevMonth = ws.Month()
		}
		for row := 0; row < 7; row++ {
			d := ws.AddDate(0, 0, row)
			ds := d.Format(time.DateOnly)
			if ds > todayStr {
				continue
			}
			v := spend[ds]
			x, y := left+col*pitch, top+row*pitch
			readout := fmt.Sprintf("%s — %s", dayLabel(ds), fmtCents(v))
			fmt.Fprintf(&b, `<g class="cell"><rect x="%d" y="%d" width="%d" height="%d" rx="4" class="heat-%d mark" aria-label="%s"/><text x="%d" y="%d" font-size="11" class="readout">%s</text></g>`,
				x, y, cell, cell, level(v), esc(readout), left, legendY+10, esc(readout))
		}
	}

	// Scale key, bottom-right.
	keyX := w - 5*16 - 74
	fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="10" class="muted" text-anchor="end">less</text>`, keyX-6, legendY+10)
	for i := 0; i < 5; i++ {
		fmt.Fprintf(&b, `<rect x="%d" y="%d" width="12" height="12" rx="3" class="heat-%d"/>`, keyX+i*16, legendY, i)
	}
	fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="10" class="muted">more</text>`, keyX+5*16+2, legendY+10)

	b.WriteString("</svg>")
	return b.String()
}

// heatCSS maps the five heat levels to theme variables. Fallbacks are the
// sequential blue ramp (light mode); the theme's dark block re-anchors them
// so near-zero recedes toward the dark surface. Hover behavior comes from
// the shared .cell rules in svgOpen.
func heatCSS() string {
	fallback := [5]string{"#e9ecef", "#cde2fb", "#9ec5f4", "#5598e7", "#256abf"}
	var b strings.Builder
	for i, fb := range fallback {
		fmt.Fprintf(&b, ".heat-%d{fill:var(--chart-heat-%d,%s)}", i, i, fb)
	}
	return b.String()
}
