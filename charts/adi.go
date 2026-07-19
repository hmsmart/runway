package charts

import (
	"fmt"
	"math"
	"strings"
)

// ADIState represents the spend attitude indicator state.
type ADIState struct {
	// Pitch in pixels — positive = nose up (under budget), negative = nose down (over budget).
	// Clamped to [-45, 45] so a visible band of sky and ground always remains.
	Pitch float64

	// BurnMTD is the month-to-date discretionary spend, formatted for display.
	BurnMTD string

	// Ticks are the pitch ladder labels showing per-day deltas.
	Ticks []ADITick
}

// ADITick is a single pitch ladder label.
type ADITick struct {
	// DY is the vertical offset from the horizon in SVG pixels.
	// Negative = sky side (savings), positive = ground side (overspend).
	DY float64

	// Label is the formatted dollar amount (e.g. "+$4.50" or "-$2.99").
	Label string

	// Sky is true for ticks above the horizon (under-budget side).
	Sky bool
}

// Fill returns the tick label's color: minty on the sky side, salmon on the
// ground side, both readable over their band.
func (t ADITick) Fill() string {
	if t.Sky {
		return "rgba(160,230,180,0.85)"
	}
	return "rgba(255,170,160,0.85)"
}

// ComputeADI builds the attitude indicator state from EMA spend data.
//
// targetDaily is the ideal daily discretionary spend to stay on budget.
// ema14 and ema28 are the 14-day and 28-day exponential moving averages of daily spend.
// burnMTD is the raw month-to-date burn in cents (or smallest unit).
func ComputeADI(targetDaily, ema14, ema28, burnMTD float64) ADIState {
	if targetDaily <= 0 {
		targetDaily = 1
	}
	// Use the 14-day EMA as the primary pitch driver since it's the most responsive.
	// Ratio: how far off target, as a fraction of target.
	// Positive ratio = under budget (nose up), negative = over budget (nose down).
	ratio := (targetDaily - ema14) / targetDaily

	// Map ratio to pitch pixels, clamped so the horizon never pins against
	// the ladder and the instrument still reads as an attitude.
	pitch := clamp(ratio*55, -45, 45)

	return ADIState{
		Pitch:   pitch,
		BurnMTD: formatUSD(burnMTD),
		Ticks:   buildTicks(targetDaily, ema14, ema28),
	}
}

// ADI renders the attitude indicator as a standalone SVG document.
func ADI(state ADIState) string {
	const (
		w, h   = 200, 200
		outerR = 100
		innerR = 94
		clipR  = 92
	)

	var b strings.Builder
	svgOpen(&b, w, h, "spend attitude indicator", `
.adi-bezel{fill:var(--chart-bar,#cdd3da)}
.adi-bezel-inner{stroke:var(--chart-heat-0,#e9ecef)}
.adi-bezel-ring{stroke:var(--chart-axis,#c6ccd4)}
.adi-sky{fill:#3a7bd5}
.adi-ground{fill:#8b6914}
`)

	b.WriteString(`<defs><clipPath id="adi-clip"><circle cx="100" cy="100" r="92"/></clipPath></defs>`)
	b.WriteString(`<g class="adi">`)
	fmt.Fprintf(&b, `<circle cx="100" cy="100" r="%d" class="adi-bezel"/>`, outerR)
	fmt.Fprintf(&b, `<circle cx="100" cy="100" r="%d" fill="none" class="adi-bezel-inner" stroke-width="2"/>`, innerR)

	fmt.Fprintf(&b, `<g clip-path="url(#adi-clip)"><g transform="translate(0,%s)">`, f(state.Pitch))
	b.WriteString(`<rect x="-100" y="-300" width="400" height="400" class="adi-sky"/>`)
	b.WriteString(`<rect x="-100" y="100" width="400" height="400" class="adi-ground"/>`)
	b.WriteString(`<line x1="-100" y1="100" x2="300" y2="100" stroke="#fff" stroke-width="2"/>`)
	b.WriteString(`<line x1="70" y1="80" x2="130" y2="80" stroke="rgba(255,255,255,0.5)" stroke-width="1"/>`)
	b.WriteString(`<line x1="75" y1="60" x2="125" y2="60" stroke="rgba(255,255,255,0.4)" stroke-width="1"/>`)
	b.WriteString(`<line x1="80" y1="40" x2="120" y2="40" stroke="rgba(255,255,255,0.3)" stroke-width="1"/>`)
	b.WriteString(`<line x1="70" y1="120" x2="130" y2="120" stroke="rgba(255,255,255,0.5)" stroke-width="1"/>`)
	b.WriteString(`<line x1="75" y1="140" x2="125" y2="140" stroke="rgba(255,255,255,0.4)" stroke-width="1"/>`)
	b.WriteString(`<line x1="80" y1="160" x2="120" y2="160" stroke="rgba(255,255,255,0.3)" stroke-width="1"/>`)
	// style= (not the fill attribute) because svgOpen's text{fill:ink} rule
	// overrides presentation attributes. Labels ride the pitch translation,
	// so drop any that would land where the round face is too narrow for
	// them — a half-clipped dollar figure is worse than none.
	for _, tick := range state.Ticks {
		if screenY := 100 + tick.DY + state.Pitch; screenY < 52 || screenY > 148 {
			continue
		}
		fmt.Fprintf(&b, `<text x="55" y="%s" text-anchor="end" font-family="'B612 Mono', monospace" font-size="9" font-weight="700" style="fill:%s">%s</text>`,
			f(100+tick.DY+4), esc(tick.Fill()), esc(tick.Label))
	}
	b.WriteString(`</g></g>`)

	// The month-to-date readout rides the reference line: green in the sky,
	// red once the reticle is on terrain (over budget).
	burnFill := "#00ff88"
	if state.Pitch < 0 {
		burnFill = "#ff5c5c"
	}
	b.WriteString(`<rect x="136" y="89" width="52" height="22" rx="3" fill="rgba(0,0,0,0.78)" stroke="rgba(255,255,255,0.45)" stroke-width="1"/>`)
	fmt.Fprintf(&b, `<text x="162" y="104.5" text-anchor="middle" font-family="'B612 Mono', monospace" font-size="12" font-weight="700" style="fill:%s">%s</text>`, burnFill, esc(state.BurnMTD))
	b.WriteString(`<line x1="28" y1="100" x2="70" y2="100" stroke="#ff8c00" stroke-width="3.5" stroke-linecap="round"/>`)
	b.WriteString(`<line x1="130" y1="100" x2="136" y2="100" stroke="#ff8c00" stroke-width="3.5" stroke-linecap="round"/>`)
	b.WriteString(`<circle cx="100" cy="100" r="4.5" fill="none" stroke="#ff8c00" stroke-width="2.5"/>`)
	fmt.Fprintf(&b, `<circle cx="100" cy="100" r="%d" fill="none" class="adi-bezel-ring" stroke-width="1" opacity="0.5"/>`, clipR)
	b.WriteString(`</g></svg>`)
	return b.String()
}

// buildTicks creates pitch ladder labels from the EMA deltas.
// Each tick represents a daily spend delta at a given interval.
func buildTicks(target, ema14, ema28 float64) []ADITick {
	var ticks []ADITick

	type emaEntry struct {
		label string
		delta float64 // positive = overspending, negative = underspending
	}

	entries := []emaEntry{
		{"14d", ema14 - target},
		{"28d", ema28 - target},
	}

	// Space ticks at 20px intervals from the horizon.
	// Overspend deltas go below (ground side), underspend above (sky side).
	spacing := 20.0
	skyIdx := 1
	gndIdx := 1

	// Labels are short signed dollars only — anything longer runs off the
	// face's left edge (the text is end-anchored against the ladder rungs).
	for _, e := range entries {
		if e.delta > 0 {
			// Over budget → ground side (positive DY).
			ticks = append(ticks, ADITick{
				DY:    spacing * float64(gndIdx),
				Label: "+" + formatUSD(e.delta),
				Sky:   false,
			})
			gndIdx++
		} else if e.delta < 0 {
			// Under budget → sky side (negative DY).
			ticks = append(ticks, ADITick{
				DY:    -spacing * float64(skyIdx),
				Label: "-" + formatUSD(math.Abs(e.delta)),
				Sky:   true,
			})
			skyIdx++
		}
		// Skip if exactly on target — no tick needed.
	}

	// Add a second tier at 2x the delta for scale reference.
	for _, e := range entries {
		if e.delta > 0 {
			ticks = append(ticks, ADITick{
				DY:    spacing * float64(gndIdx),
				Label: "+" + formatUSD(e.delta*2),
				Sky:   false,
			})
			gndIdx++
		} else if e.delta < 0 {
			ticks = append(ticks, ADITick{
				DY:    -spacing * float64(skyIdx),
				Label: "-" + formatUSD(math.Abs(e.delta*2)),
				Sky:   true,
			})
			skyIdx++
		}
	}

	return ticks
}

func formatUSD(v float64) string {
	if v >= 1000 {
		return "$" + comma(fmt.Sprintf("%.0f", v))
	}
	return fmt.Sprintf("$%.2f", v)
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
