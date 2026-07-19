package charts

import (
	"fmt"
	"strings"
)

// LampState represents the three annunciator states.
type LampState int

const (
	LampUnder LampState = iota
	LampWarn
	LampOver
)

// String returns the display text for the lamp.
func (l LampState) String() string {
	switch l {
	case LampOver:
		return "OVER"
	case LampWarn:
		return "WARN"
	default:
		return "UNDER"
	}
}

// CSS returns the modifier class for the lamp, tying it to the --lamp-*
// theme variables.
func (l LampState) CSS() string {
	switch l {
	case LampOver:
		return "lamp-over"
	case LampWarn:
		return "lamp-warn"
	default:
		return "lamp-under"
	}
}

// LampTrend represents the direction arrow for a lamp.
type LampTrend int

const (
	LampTrendFlat LampTrend = iota
	LampTrendUp             // spending increasing (bad)
	LampTrendDown           // spending decreasing (good)
)

// Arrow returns the display character.
func (t LampTrend) Arrow() string {
	switch t {
	case LampTrendUp:
		return "▲"
	case LampTrendDown:
		return "▼"
	default:
		return ""
	}
}

// Lamp is a single annunciator indicator.
type Lamp struct {
	Label string // e.g. "EMA-14", "EMA-28"
	State LampState
	Trend LampTrend
	// Value overrides State.String() as the lamp text when set — for lamps
	// (like SYNC) whose states aren't spend levels.
	Value string
}

// SpendReduction holds the per-day reduction targets when missing budget.
type SpendReduction struct {
	Target14 string // e.g. "$4.50"
	Target28 string // e.g. "$2.99"
}

// Annunciator is the full panel state.
type Annunciator struct {
	Lamp14    Lamp
	Lamp28    Lamp
	LampSync  Lamp
	Reduction *SpendReduction // nil when on target
}

// ComputeAnnunciator derives the panel state from EMA data.
//
// targetDaily: ideal daily discretionary spend.
// ema14, ema28: current EMA values.
// ema14Prev, ema28Prev: previous EMA values (for trend direction).
// daysLeft: remaining days in the budget period.
// remainingBudget: dollars left in the monthly budget.
// syncOK: whether any tracked account synced recently — a stale panel is a
// lying panel, and the SYNC lamp says so.
func ComputeAnnunciator(
	targetDaily, ema14, ema28 float64,
	ema14Prev, ema28Prev float64,
	daysLeft int, remainingBudget float64,
	syncOK bool,
) Annunciator {
	a := Annunciator{
		Lamp14:   computeLamp("EMA-14", ema14, ema14Prev, targetDaily),
		Lamp28:   computeLamp("EMA-28", ema28, ema28Prev, targetDaily),
		LampSync: Lamp{Label: "SYNC", State: LampUnder, Value: "OK"},
	}
	if !syncOK {
		a.LampSync = Lamp{Label: "SYNC", State: LampOver, Value: "STALE"}
	}

	// Show spend reduction targets if either lamp is OVER or WARN.
	if a.Lamp14.State >= LampWarn || a.Lamp28.State >= LampWarn {
		if daysLeft > 0 {
			// How much per day to get back on target.
			adjustedDaily := remainingBudget / float64(daysLeft)

			// Reduction needed relative to each EMA.
			red14 := ema14 - adjustedDaily
			red28 := ema28 - adjustedDaily

			if red14 > 0 || red28 > 0 {
				a.Reduction = &SpendReduction{
					Target14: formatUSD(max(red14, 0)),
					Target28: formatUSD(max(red28, 0)),
				}
			}
		}
	}

	return a
}

func computeLamp(label string, current, prev, target float64) Lamp {
	l := Lamp{Label: label}
	if target <= 0 {
		target = 1
	}

	// State thresholds: >10% over = OVER, >0% over = WARN, at or under = UNDER.
	ratio := (current - target) / target
	switch {
	case ratio > 0.10:
		l.State = LampOver
	case ratio > 0:
		l.State = LampWarn
	default:
		l.State = LampUnder
	}

	// Trend direction based on EMA movement.
	delta := current - prev
	switch {
	case delta > 0.01:
		l.Trend = LampTrendUp
	case delta < -0.01:
		l.Trend = LampTrendDown
	default:
		l.Trend = LampTrendFlat
	}

	return l
}

// AnnunciatorSVG renders the annunciator panel as a standalone SVG document.
func AnnunciatorSVG(a Annunciator) string {
	const w, h = 200, 200

	var b strings.Builder
	svgOpen(&b, w, h, "spend annunciator", `
.ann-title{font-size:10px;letter-spacing:1.8px}
.ann-subtitle{font-size:9px;letter-spacing:1.1px}
.ann-label{font-size:8px;letter-spacing:1.2px}
.ann-value{font-size:12px;font-weight:700}
.ann-panel{fill:var(--chart-face,#f8f9fb);stroke:var(--chart-axis,#c6ccd4);stroke-width:1}
.lamp-under{fill:var(--lamp-under,#eef5e4);stroke:var(--lamp-under-edge,#639922)}
.lamp-warn{fill:var(--lamp-warn,#fdf3e0);stroke:var(--lamp-warn-edge,#d99114)}
.lamp-over{fill:var(--lamp-over,#fce8e8);stroke:var(--lamp-over-edge,#e24b4a)}
`)

	b.WriteString(`<g>`)
	b.WriteString(`<rect x="8" y="8" width="184" height="184" rx="10" class="ann-panel"/>`)
	b.WriteString(`<text x="100" y="24" text-anchor="middle" class="ann-title muted">SPEND STATUS</text>`)

	renderLamp := func(x float64, l Lamp) {
		const lampW = 54.0
		fmt.Fprintf(&b, `<rect x="%s" y="34" width="%s" height="56" rx="6" class="%s" stroke-width="1.5"/>`, f(x), f(lampW), l.State.CSS())
		fmt.Fprintf(&b, `<text x="%s" y="47" text-anchor="middle" class="ann-label muted">%s</text>`, f(x+lampW/2), esc(l.Label))
		status := l.Value
		if status == "" {
			status = l.State.String()
			if arrow := l.Trend.Arrow(); arrow != "" {
				status += " " + arrow
			}
		}
		fmt.Fprintf(&b, `<text x="%s" y="70" text-anchor="middle" class="ann-value">%s</text>`, f(x+lampW/2), esc(status))
	}

	renderLamp(16, a.Lamp14)
	renderLamp(73, a.Lamp28)
	renderLamp(130, a.LampSync)

	if a.Reduction != nil {
		b.WriteString(`<text x="100" y="110" text-anchor="middle" class="ann-subtitle muted">SPEND REDUCTION</text>`)
		b.WriteString(`<rect x="18" y="118" width="78" height="58" rx="6" fill="var(--chart-bg,#fff)" stroke="var(--chart-axis,#c6ccd4)"/>`)
		b.WriteString(`<rect x="104" y="118" width="78" height="58" rx="6" fill="var(--chart-bg,#fff)" stroke="var(--chart-axis,#c6ccd4)"/>`)
		fmt.Fprintf(&b, `<text x="57" y="132" text-anchor="middle" class="ann-label muted">14-DAY</text><text x="57" y="154" text-anchor="middle" class="ann-value">%s</text><text x="57" y="168" text-anchor="middle" class="ann-label muted">PER DAY</text>`, esc(a.Reduction.Target14))
		fmt.Fprintf(&b, `<text x="143" y="132" text-anchor="middle" class="ann-label muted">28-DAY</text><text x="143" y="154" text-anchor="middle" class="ann-value">%s</text><text x="143" y="168" text-anchor="middle" class="ann-label muted">PER DAY</text>`, esc(a.Reduction.Target28))
	} else {
		b.WriteString(`<text x="100" y="136" text-anchor="middle" class="ann-subtitle muted">ON GLIDESLOPE</text>`)
		b.WriteString(`<text x="100" y="152" text-anchor="middle" class="ann-label muted">NO REDUCTION REQUIRED</text>`)
	}

	b.WriteString(`</g></svg>`)
	return b.String()
}
