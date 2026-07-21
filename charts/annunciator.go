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

func (l LampState) readoutColor() string {
	switch l {
	case LampOver:
		return "#e24b4a"
	case LampWarn:
		return "#d99114"
	default:
		return "#00ff88"
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
	LampFuel  Lamp
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
	fuelWarn, fuelLow bool,
) Annunciator {
	a := Annunciator{
		Lamp14:   computeLamp("EMA-14", ema14, ema14Prev, targetDaily),
		Lamp28:   computeLamp("EMA-28", ema28, ema28Prev, targetDaily),
		LampSync: Lamp{Label: "SYNC", State: LampUnder, Value: "OK"},
		LampFuel: Lamp{Label: "FUEL", State: LampUnder, Value: "OK"},
	}
	if !syncOK {
		a.LampSync = Lamp{Label: "SYNC", State: LampOver, Value: "STALE"}
	}
	if fuelLow {
		a.LampFuel = Lamp{Label: "FUEL", State: LampOver, Value: "LOW"}
	} else if fuelWarn {
		a.LampFuel = Lamp{Label: "FUEL", State: LampWarn, Value: "WARN"}
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


// casLamp renders a single CAS lamp at position (x, y) with size 80x52.
// Dark when UNDER, amber-lit when WARN, red-lit when OVER.
func casLamp(b *strings.Builder, x, y float64, l Lamp) {
	const lampW, lampH = 80.0, 52.0

	status := l.Value
	if status == "" {
		status = l.State.String()
		if arrow := l.Trend.Arrow(); arrow != "" {
			status += " " + arrow
		}
	}

	switch l.State {
	case LampOver:
		fmt.Fprintf(b, `<rect x="%s" y="%s" width="%s" height="%s" rx="3" fill="#3a1c1c" stroke="#5a2a2a" stroke-width="1.5"/>`, f(x), f(y), f(lampW), f(lampH))
		fmt.Fprintf(b, `<text x="%s" y="%s" text-anchor="middle" font-size="7" style="fill:#e24b4a;opacity:0.8;font-family:'B612 Mono',var(--chart-font,system-ui,sans-serif);letter-spacing:1.5px">%s</text>`, f(x+lampW/2), f(y+14), esc(l.Label))
		fmt.Fprintf(b, `<text x="%s" y="%s" text-anchor="middle" font-size="15" style="fill:#e24b4a;font-family:'B612 Mono',var(--chart-font,system-ui,sans-serif);font-weight:700">%s</text>`, f(x+lampW/2), f(y+38), esc(status))
	case LampWarn:
		fmt.Fprintf(b, `<rect x="%s" y="%s" width="%s" height="%s" rx="3" fill="#362a10" stroke="#5a4520" stroke-width="1.5"/>`, f(x), f(y), f(lampW), f(lampH))
		fmt.Fprintf(b, `<text x="%s" y="%s" text-anchor="middle" font-size="7" style="fill:#d99114;opacity:0.8;font-family:'B612 Mono',var(--chart-font,system-ui,sans-serif);letter-spacing:1.5px">%s</text>`, f(x+lampW/2), f(y+14), esc(l.Label))
		fmt.Fprintf(b, `<text x="%s" y="%s" text-anchor="middle" font-size="15" style="fill:#d99114;font-family:'B612 Mono',var(--chart-font,system-ui,sans-serif);font-weight:700">%s</text>`, f(x+lampW/2), f(y+38), esc(status))
	default:
		fmt.Fprintf(b, `<rect x="%s" y="%s" width="%s" height="%s" rx="3" fill="#1a1e25" stroke="#262b33" stroke-width="1"/>`, f(x), f(y), f(lampW), f(lampH))
		fmt.Fprintf(b, `<text x="%s" y="%s" text-anchor="middle" font-size="7" style="fill:#333a44;font-family:'B612 Mono',var(--chart-font,system-ui,sans-serif);letter-spacing:1.5px">%s</text>`, f(x+lampW/2), f(y+14), esc(l.Label))
		fmt.Fprintf(b, `<text x="%s" y="%s" text-anchor="middle" font-size="15" style="fill:#333a44;font-family:'B612 Mono',var(--chart-font,system-ui,sans-serif);font-weight:700">%s</text>`, f(x+lampW/2), f(y+38), esc(status))
	}
}
