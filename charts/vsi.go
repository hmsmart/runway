package charts

import (
	"fmt"
	"math"
	"strings"
)

// GaugeScale describes the three breakpoints used by the VSI dial.
// Inner is the dense linear region, Mid is the bend, and Outer is the
// clamp point where the needle stops.
type GaugeScale struct {
	Inner float64
	Mid   float64
	Outer float64
}

var defaultGaugeScale = GaugeScale{Inner: 5, Mid: 10, Outer: 50}

// VSI renders a vertical-speed-style gauge for a current value and a sample
// series used to derive the scale breakpoints. Percentiles are taken over the
// absolute values of the series, then rounded to a stable display grid.
func VSI(values []float64, current float64) string {
	scale := gaugeScaleFromValues(values)
	return renderVSI(current, scale)
}

func gaugeScaleFromValues(values []float64) GaugeScale {
	if len(values) == 0 {
		return defaultGaugeScale
	}
	p85, err := percentileInc(values, 85)
	if err != nil {
		return defaultGaugeScale
	}
	p95, err := percentileInc(values, 95)
	if err != nil {
		return defaultGaugeScale
	}
	p995, err := percentileInc(values, 99.5)
	if err != nil {
		return defaultGaugeScale
	}

	// Runway-day deltas are usually much smaller than spend deltas, so use a
	// finer rounding grid for stable-but-readable breakpoints.
	innerStep, midStep, outerStep := 0.5, 0.5, 1.0
	maxAbs := math.Max(math.Abs(p95), math.Abs(p995))
	switch {
	case maxAbs < 1:
		innerStep, midStep, outerStep = 0.05, 0.1, 0.25
	case maxAbs < 5:
		innerStep, midStep, outerStep = 0.1, 0.25, 0.5
	}

	return normalizeGaugeScale(
		roundToStep(p85, innerStep),
		roundToStep(p95, midStep),
		roundToStep(p995, outerStep),
	)
}

func normalizeGaugeScale(inner, mid, outer float64) GaugeScale {
	inner = math.Max(0.5, math.Abs(inner))
	mid = math.Max(inner+0.5, math.Abs(mid))
	outer = math.Max(mid+0.5, math.Abs(outer))
	return GaugeScale{Inner: inner, Mid: mid, Outer: outer}
}

func roundToStep(v, step float64) float64 {
	if step <= 0 {
		return v
	}
	return math.Round(v/step) * step
}

func fmtTick(v float64) string {
	rounded := math.Round(v)
	if math.Abs(v-rounded) < 0.001 {
		return fmt.Sprintf("%.0f", rounded)
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", v), ".0")
}

// gaugeValueToAngle maps a VSI value to a screen angle using a 3-segment
// piecewise-linear scale: the inner region is dense (most values live here),
// the mid region compresses, and the outer region clamps extremes.
//
// The full sweep is 320° centered on 9 o'clock (180°):
//   - Positive (spend falling, good): 180° → 340° (counter-clockwise up-left)
//   - Negative (spend rising, bad):   180° → 20° (clockwise down-left)
//   - Zero:                           180° (9 o'clock, horizontal left)
func gaugeValueToAngle(v float64, scale GaugeScale) float64 {
	clamped := math.Max(-scale.Outer, math.Min(scale.Outer, v))
	// Flip sign so positive deltas render on the UP side of the dial.
	mapped := -clamped
	absV := math.Abs(mapped)
	sign := 1.0
	if mapped < 0 {
		sign = -1
	}

	if absV <= scale.Inner {
		return 180 - (mapped/scale.Inner)*90
	}
	if absV <= scale.Mid {
		span := (absV - scale.Inner) / (scale.Mid - scale.Inner)
		if sign > 0 {
			return 90 - span*30
		}
		return 270 + span*30
	}
	span := (absV - scale.Mid) / (scale.Outer - scale.Mid)
	if sign > 0 {
		return 60 - span*40
	}
	return 300 + span*40
}

func gaugeTicks(scale GaugeScale) ([]float64, []float64) {
	appendSym := func(values []float64, v float64) []float64 {
		if v <= 0 {
			return values
		}
		values = append(values, v, -v)
		return values
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 0.001 }
	contains := func(values []float64, v float64) bool {
		for _, x := range values {
			if near(x, v) {
				return true
			}
		}
		return false
	}

	// Deterministic markers by segment: keep dense ticks in the inner region
	// (where the dial allocates the most angular resolution) and sparse ticks
	// beyond the bend.
	majors := []float64{0}
	majors = appendSym(majors, scale.Inner)
	majors = appendSym(majors, scale.Mid)
	majors = appendSym(majors, scale.Outer)

	var minors []float64
	if scale.Inner > 0 {
		// Even subdivisions in the high-resolution inner band (0..p85).
		innerDivs := 6
		if scale.Inner <= 1 {
			innerDivs = 5
		} else if scale.Inner >= 3 {
			innerDivs = 8
		}
		step := scale.Inner / float64(innerDivs)
		for i := 1; i < innerDivs; i++ {
			v := step * float64(i)
			// Alternate major/minor to keep labels readable while preserving
			// equal spacing for all ticks.
			if i%2 == 0 {
				if !contains(majors, v) {
					majors = appendSym(majors, v)
				}
				continue
			}
			if contains(majors, v) {
				continue
			}
			minors = appendSym(minors, v)
		}
	}

	return majors, minors
}

func signedDailyValue(v float64) string {
	if v == 0 {
		return "0.00"
	}
	if v > 0 {
		return fmt.Sprintf("▼%.2f", v)
	}
	return fmt.Sprintf("▲%.2f", math.Abs(v))
}

func renderVSI(current float64, scale GaugeScale) string {
	const (
		w, h   = 200, 200
		cx     = 100.0
		cy     = 100.0
		bandR  = 84.0
		needle = 66.0
	)

	var b strings.Builder
	svgOpen(&b, w, h, "vertical speed indicator - runway days trend delta", `
.vsi .readout-window{fill:var(--chart-heat-0,#e9ecef);stroke:var(--chart-axis,#c6ccd4);stroke-width:1.5}
.vsi .readout{fill:var(--chart-ink,#33383f);font-family:'B612 Mono',var(--chart-font,system-ui,sans-serif);font-size:13px;font-weight:700;letter-spacing:0.02em}
.vsi .readout-unit{font-size:9px;font-weight:600;letter-spacing:0}
`)
	b.WriteString(`<g class="vsi">`)
	bezel(&b, cx, cy)

	// Arc bands: green on the UP side (spend falling = good), amber in shallow
	// DN, and red on strong DN (spend rising = bad). bandArc draws clockwise,
	// so each segment is emitted with ascending screen angles.
	upOuter := gaugeValueToAngle(scale.Outer, scale)
	upInner := gaugeValueToAngle(scale.Inner, scale)
	zero := gaugeValueToAngle(0, scale)
	dnInner := gaugeValueToAngle(-scale.Inner, scale)
	dnOuter := gaugeValueToAngle(-scale.Outer, scale)

	bandArc(&b, cx, cy, bandR, upInner, upOuter, 6, gaugeGreen)
	bandArc(&b, cx, cy, bandR, zero, upInner, 6, gaugeGreen)
	bandArc(&b, cx, cy, bandR, dnInner, zero, 6, gaugeAmber)
	bandArc(&b, cx, cy, bandR, dnOuter, dnInner, 6, gaugeRed)

	// Major and minor tick marks.
	majors, minors := gaugeTicks(scale)
	near := func(a, b float64) bool { return math.Abs(a-b) < 0.001 }
	for _, v := range majors {
		deg := gaugeValueToAngle(v, scale)
		length, wgt := 5.0, 1.0
		if v == 0 || math.Abs(math.Abs(v)-scale.Inner) < 0.001 {
			length, wgt = 9, 2
		}
		radialTick(&b, cx, cy, bandR-length, bandR, deg,
			fmt.Sprintf(`stroke="var(--chart-ink,#33383f)" stroke-width="%s" stroke-linecap="round"`, f(wgt)))
		absV := math.Abs(v)
		showLabel := false
		switch {
		case near(absV, 0):
			showLabel = true
		case near(absV, scale.Inner):
			// Always label the inner breakpoint; it should sit on a major tick.
			showLabel = true
		case absV < scale.Inner-0.001:
			// Left-half detailed labels.
			showLabel = true
		case near(absV, scale.Mid), near(absV, scale.Outer):
			// Right-half labels only at p95 and p99.5 breakpoints.
			showLabel = true
		}
		if showLabel {
			// Match fuel gauge label placement so numbers sit closer to ticks.
			tx, ty := polar(cx, cy, bandR-19, deg)
			label := fmtTick(absV)
			fmt.Fprintf(&b, `<text x="%s" y="%s" font-size="10" font-weight="700" text-anchor="middle" class="muted">%s</text>`,
				f(tx), f(ty+3), esc(label))
		}
	}
	for _, v := range minors {
		deg := gaugeValueToAngle(v, scale)
		radialTick(&b, cx, cy, bandR-6, bandR, deg,
			`stroke="var(--chart-ink,#33383f)" stroke-width="1" stroke-linecap="round" opacity="0.8"`)
	}

	// Title and subtitle, matching ASI/Fuel/Corrective style.
	b.WriteString(`<text x="100" y="58" text-anchor="middle" font-size="10" font-weight="700" class="muted" letter-spacing="0.14em">VSI</text>`)
	b.WriteString(`<text x="100" y="70" text-anchor="middle" font-size="8" class="muted" letter-spacing="0.08em">DELTA RUNWAY DAYS</text>`)

	// UP/DN hints near the arc ends.
	upDeg := gaugeValueToAngle(scale.Inner*0.6, scale)
	dnDeg := gaugeValueToAngle(-scale.Inner*0.6, scale)
	upX, upY := polar(cx, cy, bandR-30, upDeg)
	dnX, dnY := polar(cx, cy, bandR-30, dnDeg)
	fmt.Fprintf(&b, `<text x="%s" y="%s" text-anchor="middle" font-size="7.5" font-weight="700" style="fill:%s" letter-spacing="0.1em">INC</text>`,
		f(upX), f(upY+3), gaugeGreen)
	fmt.Fprintf(&b, `<text x="%s" y="%s" text-anchor="middle" font-size="7.5" font-weight="700" style="fill:%s" letter-spacing="0.1em">DEC</text>`,
		f(dnX), f(dnY+3), gaugeRed)

	// Needle — ink-colored, matching ASI/Fuel/Corrective.
	// Needle direction is intentionally mirrored from raw value so motion
	// matches runway-day delta semantics on this dial orientation.
	needleDeg := gaugeValueToAngle(-current, scale)
	needleLine(&b, cx, cy, needle, needleDeg,
		`stroke="var(--chart-ink,#33383f)" stroke-width="2.5" stroke-linecap="round"`)
	hub(&b, cx, cy)

	// Readout window slightly below the hub center for better balance.
	b.WriteString(`<rect x="52" y="110" width="96" height="22" rx="4" class="readout-window"/>`)
	fmt.Fprintf(&b, `<text x="100" y="125" text-anchor="middle" class="readout"><tspan>%s</tspan><tspan class="readout-unit"> d/day</tspan></text>`,
		esc(signedDailyValue(current)))

	b.WriteString(`</g></svg>`)
	return b.String()
}
