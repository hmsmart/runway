package charts

import (
	"fmt"
	"math"
	"strings"
)

// Fuel gauge scale: the dial reads 0..fuelFull days of runway across a
// half-circle sweep, with warning bands at the aviation-style thresholds.
// Under fuelRed days is the red arc, under fuelAmber the caution arc, and
// everything above is green. Runway beyond full scale just pegs the needle
// at F — by then the exact number stops being interesting.
const (
	fuelFull  = 120.0
	fuelRed   = 30.0
	fuelAmber = 60.0
)

// The dial is concentric with the face: only then can its radius run all the
// way out to the bezel (apex at y=12) while the half-circle's ends still land
// inside the face. A lowered hub caps the radius far short of the top.
const (
	fuelCX, fuelCY = 100.0, 100.0
	fuelBandR      = 88.0
)

// fuelOpen starts the fuel SVG and draws the shared dial: bezel, bands,
// ticks, and placards. Fuel and FuelINOP finish it differently.
func fuelOpen(b *strings.Builder) {
	svgOpen(b, 200, 200, "fuel gauge — days of runway", `
.fuel .readout-window{fill:var(--chart-heat-0,#e9ecef);stroke:var(--chart-axis,#c6ccd4);stroke-width:1.5}
.fuel .readout{fill:var(--chart-ink,#33383f);font-family:'B612 Mono',var(--chart-font,system-ui,sans-serif);font-size:13px;font-weight:700;letter-spacing:0.02em}
.fuel .readout-unit{font-size:9px;font-weight:600;letter-spacing:0}
.fuel .n14{stroke:var(--chart-ema14,#6da7ec)}
.fuel .n28{stroke:var(--chart-ema28,#256abf)}
`)
	b.WriteString(`<g class="fuel">`)
	bezel(b, fuelCX, fuelCY)

	// Scale bands: red, amber, green across the half-circle.
	bandArc(b, fuelCX, fuelCY, fuelBandR, fuelAngle(0), fuelAngle(fuelRed), 6, gaugeRed)
	bandArc(b, fuelCX, fuelCY, fuelBandR, fuelAngle(fuelRed), fuelAngle(fuelAmber), 6, gaugeAmber)
	bandArc(b, fuelCX, fuelCY, fuelBandR, fuelAngle(fuelAmber), fuelAngle(fuelFull), 6, gaugeGreen)

	// Ticks: minors every 10 days, majors every 30 with labels.
	for d := 0.0; d <= fuelFull; d += 10 {
		major := math.Mod(d, 30) == 0
		length, wgt := 5.0, 1.0
		if major {
			length, wgt = 9, 2
		}
		radialTick(b, fuelCX, fuelCY, fuelBandR-length, fuelBandR, fuelAngle(d),
			fmt.Sprintf(`stroke="var(--chart-ink,#33383f)" stroke-width="%s" stroke-linecap="round"`, f(wgt)))
		if major && d > 0 && d < fuelFull {
			tx, ty := polar(fuelCX, fuelCY, fuelBandR-19, fuelAngle(d))
			fmt.Fprintf(b, `<text x="%s" y="%s" font-size="9" font-weight="700" text-anchor="middle" class="muted">%.0f</text>`, f(tx), f(ty+3), d)
		}
	}
	// E and F placards below the band ends.
	b.WriteString(`<text x="22" y="124" font-size="13" font-weight="700" text-anchor="middle">E</text>`)
	b.WriteString(`<text x="178" y="124" font-size="13" font-weight="700" text-anchor="middle">F</text>`)
	b.WriteString(`<text x="100" y="58" text-anchor="middle" font-size="10" font-weight="700" class="muted" letter-spacing="0.14em">FUEL</text>`)
	b.WriteString(`<text x="100" y="70" text-anchor="middle" font-size="8" class="muted" letter-spacing="0.08em">DAYS OF RUNWAY</text>`)
}

// Fuel renders the days-of-runway gauge: cash on hand divided by the smoothed
// daily burn. Two needles, one per EMA horizon, wearing the site-wide horizon
// colors. Pass math.Inf(1) for a horizon whose burn rate is zero (cash never
// runs out); negative days clamp to empty.
func Fuel(days14, days28 float64) string {
	const cx, cy = fuelCX, fuelCY
	var b strings.Builder
	fuelOpen(&b)

	// Needle legend, horizon colors matching every other chart.
	b.WriteString(`<line x1="60" y1="136" x2="72" y2="136" stroke-width="2.5" class="n14"/>`)
	b.WriteString(`<text x="76" y="139" font-size="8" class="muted">14d</text>`)
	b.WriteString(`<line x1="106" y1="136" x2="118" y2="136" stroke-width="2.5" class="n28"/>`)
	b.WriteString(`<text x="122" y="139" font-size="8" class="muted">28d</text>`)

	needleLine(&b, cx, cy, 60, fuelAngle(fuelClamp(days28)), `class="n28" stroke-width="2" stroke-linecap="round" opacity="0.85"`)
	needleLine(&b, cx, cy, 74, fuelAngle(fuelClamp(days14)), `class="n14" stroke-width="2.5" stroke-linecap="round"`)
	hub(&b, cx, cy)

	b.WriteString(`<rect x="52" y="148" width="96" height="24" rx="4" class="readout-window"/>`)
	fmt.Fprintf(&b, `<text x="100" y="164" text-anchor="middle" class="readout"><tspan>%s</tspan><tspan class="readout-unit"> DAYS</tspan></text>`, esc(fuelReadout(days14)))

	b.WriteString(`</g></svg>`)
	return b.String()
}

// FuelINOP renders the fuel dial flagged inoperative — the honest display
// when no cash (depository) account is linked, so days of runway can't be
// computed. Tracking only a credit card lands here.
func FuelINOP() string {
	var b strings.Builder
	fuelOpen(&b)

	fmt.Fprintf(&b, `<rect x="70" y="88" width="60" height="26" rx="4" fill="%s"/>`, gaugeRed)
	b.WriteString(`<text x="100" y="105" text-anchor="middle" font-size="12" font-weight="700" style="fill:#fff" letter-spacing="0.14em">INOP</text>`)
	b.WriteString(`<text x="100" y="160" text-anchor="middle" font-size="9" class="muted">no cash account linked</text>`)

	b.WriteString(`</g></svg>`)
	return b.String()
}

// fuelAngle maps days on the dial to a screen angle on the 180..360 sweep.
func fuelAngle(days float64) float64 {
	return 180 + 180*(days/fuelFull)
}

func fuelClamp(days float64) float64 {
	if math.IsInf(days, 1) {
		return fuelFull
	}
	return clamp(days, 0, fuelFull)
}

func fuelReadout(days float64) string {
	if math.IsInf(days, 1) {
		return "∞"
	}
	if days <= 0 {
		return "0"
	}
	return comma(fmt.Sprintf("%.0f", days))
}
