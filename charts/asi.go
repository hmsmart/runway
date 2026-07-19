package charts

import (
	"fmt"
	"math"
	"strings"
)

// ASI renders the airspeed indicator: current burn velocity (a short trailing
// mean of daily spend) on a 270° dial whose arcs anchor to the daily target.
// Green runs to the target, amber from there to redline at 1.5×, red beyond —
// the pace at which the month's budget dies early. Speed can peg past the
// dial max; the readout still shows the true figure.
func ASI(speed, target float64) string {
	if target <= 0 {
		target = 1
	}
	vMax := niceMax(2 * target)
	redline := math.Min(1.5*target, vMax)

	const (
		cx, cy = 100.0, 100.0
		bandR  = 84.0
	)
	// 135° (down-left) sweeping 270° over the top to 45° (down-right).
	angle := func(v float64) float64 {
		return 135 + 270*clamp(v/vMax, 0, 1)
	}

	var b strings.Builder
	svgOpen(&b, 200, 200, "airspeed indicator — daily burn rate", `
.asi .readout-window{fill:var(--chart-heat-0,#e9ecef);stroke:var(--chart-axis,#c6ccd4);stroke-width:1.5}
.asi .readout{fill:var(--chart-ink,#33383f);font-family:'B612 Mono',var(--chart-font,system-ui,sans-serif);font-size:13px;font-weight:700;letter-spacing:0.02em}
.asi .readout-unit{font-size:9px;font-weight:600;letter-spacing:0}
`)
	b.WriteString(`<g class="asi">`)
	bezel(&b, cx, cy)

	bandArc(&b, cx, cy, bandR, angle(0), angle(target), 6, gaugeGreen)
	if redline > target {
		bandArc(&b, cx, cy, bandR, angle(target), angle(redline), 6, gaugeAmber)
	}
	if vMax > redline {
		bandArc(&b, cx, cy, bandR, angle(redline), angle(vMax), 6, gaugeRed)
	}
	// The redline itself, a heavy red radial tick.
	radialTick(&b, cx, cy, bandR-10, bandR+6, angle(redline),
		fmt.Sprintf(`stroke="%s" stroke-width="3"`, gaugeRed))

	// Ticks at eighths of the dial, labeled at the quarters.
	for i := 0; i <= 8; i++ {
		v := vMax * float64(i) / 8
		major := i%2 == 0
		length, wgt := 5.0, 1.0
		if major {
			length, wgt = 9, 2
		}
		radialTick(&b, cx, cy, bandR-length, bandR, angle(v),
			fmt.Sprintf(`stroke="var(--chart-ink,#33383f)" stroke-width="%s" stroke-linecap="round"`, f(wgt)))
		// The 0 and max labels would sit under the readout window; the E-side
		// and F-side ends read fine unlabeled, like a real dial's stops.
		if major && i > 0 && i < 8 {
			tx, ty := polar(cx, cy, bandR-22, angle(v))
			fmt.Fprintf(&b, `<text x="%s" y="%s" font-size="9" font-weight="700" text-anchor="middle" class="muted">%s</text>`,
				f(tx), f(ty+3), esc(fmtWhole(v)))
		}
	}

	b.WriteString(`<text x="100" y="58" text-anchor="middle" font-size="10" font-weight="700" class="muted" letter-spacing="0.14em">AIRSPEED</text>`)
	b.WriteString(`<text x="100" y="70" text-anchor="middle" font-size="8" class="muted" letter-spacing="0.08em">BURN $ PER DAY</text>`)

	needleLine(&b, cx, cy, 66, angle(speed), `stroke="var(--chart-ink,#33383f)" stroke-width="2.5" stroke-linecap="round"`)
	hub(&b, cx, cy)

	b.WriteString(`<rect x="56" y="140" width="88" height="22" rx="4" class="readout-window"/>`)
	fmt.Fprintf(&b, `<text x="100" y="155" text-anchor="middle" class="readout"><tspan>%s</tspan><tspan class="readout-unit">/day</tspan></text>`,
		esc(fmtWhole(speed)))

	b.WriteString(`</g></svg>`)
	return b.String()
}
