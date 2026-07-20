package charts

import (
	"fmt"
	"math"
	"strings"
)

// CorrectiveState is the input for the corrective-action gauge.
type CorrectiveState struct {
	// Reduction is the dollars per day the user needs to cut to stay on
	// budget. Zero or negative means on target.
	Reduction float64

	// MaxScale is the gauge's full-scale value in dollars. Defaults to 10
	// when zero; the handler should set it to something meaningful (e.g.
	// the daily target itself, so full deflection = "stop spending entirely").
	MaxScale float64
}

// CorrectiveSVG renders the corrective-action gauge as a round 200x200 SVG
// instrument matching the ASI/Fuel/VSI bezel style. A 270-degree sweep dial with
// green/amber/red arc bands shows how much per-day spending reduction is
// needed. The needle sits at zero when on target.
func CorrectiveSVG(s CorrectiveState) string {
	const (
		cx, cy = 100.0, 100.0
		bandR  = 84.0
	)

	maxV := s.MaxScale
	if maxV <= 0 {
		maxV = 10
	}
	maxV = niceScale(maxV)

	// 270-degree sweep: 135 degrees (down-left) to 405 degrees (down-right), same as ASI.
	angle := func(v float64) float64 {
		return 135 + 270*clamp(v/maxV, 0, 1)
	}

	// Bands: green 0-33%, amber 33-67%, red 67-100% of max.
	greenEnd := maxV / 3
	amberEnd := maxV * 2 / 3

	var b strings.Builder
	svgOpen(&b, 200, 200, "corrective action - spend reduction needed", `
.cor .readout-window{fill:var(--chart-heat-0,#e9ecef);stroke:var(--chart-axis,#c6ccd4);stroke-width:1.5}
.cor .readout{fill:var(--chart-ink,#33383f);font-family:'B612 Mono',var(--chart-font,system-ui,sans-serif);font-size:13px;font-weight:700;letter-spacing:0.02em}
.cor .readout-unit{font-size:9px;font-weight:600;letter-spacing:0}
`)
	b.WriteString(`<g class="cor">`)
	bezel(&b, cx, cy)

	// Arc bands.
	bandArc(&b, cx, cy, bandR, angle(0), angle(greenEnd), 6, gaugeGreen)
	bandArc(&b, cx, cy, bandR, angle(greenEnd), angle(amberEnd), 6, gaugeAmber)
	bandArc(&b, cx, cy, bandR, angle(amberEnd), angle(maxV), 6, gaugeRed)

	// Ticks: at 0, each fifth of the scale, and max. Label every other.
	steps := 5
	for i := 0; i <= steps; i++ {
		v := maxV * float64(i) / float64(steps)
		major := i%2 == 0 || i == steps
		length, wgt := 5.0, 1.0
		if major {
			length, wgt = 9, 2
		}
		radialTick(&b, cx, cy, bandR-length, bandR, angle(v),
			fmt.Sprintf(`stroke="var(--chart-ink,#33383f)" stroke-width="%s" stroke-linecap="round"`, f(wgt)))
		if major {
			tx, ty := polar(cx, cy, bandR-22, angle(v))
			label := fmtWhole(v)
			// The 0 and max labels at the bottom of the arc need special
			// anchoring to stay clear of the readout window.
			anchor := "middle"
			if i == 0 {
				anchor = "end"
			} else if i == steps {
				anchor = "start"
			}
			fmt.Fprintf(&b, `<text x="%s" y="%s" font-size="9" font-weight="700" text-anchor="%s" class="muted">%s</text>`,
				f(tx), f(ty+3), anchor, esc(label))
		}
	}

	// Labels.
	b.WriteString(`<text x="100" y="58" text-anchor="middle" font-size="10" font-weight="700" class="muted" letter-spacing="0.14em">CORRECTIVE</text>`)
	b.WriteString(`<text x="100" y="70" text-anchor="middle" font-size="8" class="muted" letter-spacing="0.08em">REDUCE $ PER DAY</text>`)

	// Needle.
	val := clamp(s.Reduction, 0, maxV)
	needleLine(&b, cx, cy, 66, angle(val), `stroke="var(--chart-ink,#33383f)" stroke-width="2.5" stroke-linecap="round"`)
	hub(&b, cx, cy)

	// Readout window.
	b.WriteString(`<rect x="52" y="148" width="96" height="22" rx="4" class="readout-window"/>`)
	if s.Reduction <= 0 {
		b.WriteString(`<text x="100" y="163" text-anchor="middle" class="readout"><tspan>$0</tspan><tspan class="readout-unit"> OK</tspan></text>`)
	} else {
		fmt.Fprintf(&b, `<text x="100" y="163" text-anchor="middle" class="readout"><tspan>%s</tspan><tspan class="readout-unit">/day</tspan></text>`,
			esc(fmtCents(s.Reduction)))
	}

	b.WriteString(`</g></svg>`)
	return b.String()
}

// correctiveReduction computes the per-day spending reduction needed to
// finish the month on budget. Returns zero when on target or ahead.
func correctiveReduction(ema14, remainingBudget float64, daysLeft int) float64 {
	if daysLeft <= 0 {
		return 0
	}
	adjustedDaily := remainingBudget / float64(daysLeft)
	reduction := ema14 - adjustedDaily
	return math.Max(reduction, 0)
}

// niceScale rounds a positive max value up to a clean 1/2/5x10^n number.
func niceScale(v float64) float64 {
	if v <= 0 {
		return 10
	}
	mag := math.Pow(10, math.Floor(math.Log10(v)))
	norm := v / mag
	step := 10.0
	switch {
	case norm <= 1:
		step = 1
	case norm <= 2:
		step = 2
	case norm <= 5:
		step = 5
	}
	return step * mag
}
