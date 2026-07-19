package charts

import (
	"fmt"
	"strings"
)

// Totalizer is the marker-beacon-plus-hour-meter instrument: three lamps
// that light as the month's budget is consumed, over an odometer of lifetime
// tracked spend.
type Totalizer struct {
	// HasBudget is false when no monthly budget is set; the marker lamps
	// stay dark and say so.
	HasBudget bool

	// Consumed is the fraction of the monthly budget burned so far (can
	// exceed 1). Markers light at 0.50, 0.75, and 0.90.
	Consumed float64

	// TotalBurn is lifetime tracked discretionary spend, in dollars.
	TotalBurn float64
}

// totMarkers are the beacon thresholds, wearing the outer/middle/inner
// marker letters. Colors follow the real lamps (blue, amber) until the
// inner marker, which trades aviation's white for red — 90% burned is a
// warning here, not a threshold crossing.
var totMarkers = [3]struct {
	letter string
	pct    float64
	color  string
}{
	{"O", 0.50, "#3f74c9"},
	{"M", 0.75, gaugeAmber},
	{"I", 0.90, gaugeRed},
}

// TotalizerSVG renders the instrument as a standalone SVG document.
func TotalizerSVG(t Totalizer) string {
	var b strings.Builder
	svgOpen(&b, 200, 200, "totalizer — budget markers and total burn", `
.tot-panel{fill:var(--chart-face,#f8f9fb);stroke:var(--chart-axis,#c6ccd4);stroke-width:1}
.tot-title{font-size:10px;letter-spacing:1.8px}
.tot-label{font-size:8px;letter-spacing:1.2px}
.tot-digit{fill:#e8ecef;font-family:'B612 Mono',monospace;font-size:16px;font-weight:700}
`)
	b.WriteString(`<g>`)
	b.WriteString(`<rect x="8" y="8" width="184" height="184" rx="10" class="tot-panel"/>`)
	b.WriteString(`<text x="100" y="26" text-anchor="middle" class="tot-title muted">TOTALIZER</text>`)

	for i, m := range totMarkers {
		cx := 50.0 + 50*float64(i)
		lit := t.HasBudget && t.Consumed >= m.pct
		if lit {
			fmt.Fprintf(&b, `<circle cx="%s" cy="62" r="13" fill="%s"/>`, f(cx), m.color)
			fmt.Fprintf(&b, `<text x="%s" y="67" text-anchor="middle" font-size="12" font-weight="700" style="fill:#fff">%s</text>`, f(cx), m.letter)
		} else {
			fmt.Fprintf(&b, `<circle cx="%s" cy="62" r="13" fill="var(--chart-bg,#fff)" stroke="var(--chart-axis,#c6ccd4)" stroke-width="1.5"/>`, f(cx))
			fmt.Fprintf(&b, `<text x="%s" y="67" text-anchor="middle" font-size="12" font-weight="700" class="muted">%s</text>`, f(cx), m.letter)
		}
		fmt.Fprintf(&b, `<text x="%s" y="88" text-anchor="middle" class="tot-label muted">%.0f%%</text>`, f(cx), m.pct*100)
	}
	if t.HasBudget {
		b.WriteString(`<text x="100" y="106" text-anchor="middle" class="tot-label muted">BUDGET CONSUMED</text>`)
	} else {
		b.WriteString(`<text x="100" y="106" text-anchor="middle" class="tot-label muted">NO BUDGET SET</text>`)
	}

	// The hour meter: lifetime burn on a digit drum.
	b.WriteString(`<text x="100" y="130" text-anchor="middle" class="tot-label muted">TOTAL BURN $</text>`)
	b.WriteString(`<rect x="30" y="138" width="140" height="34" rx="4" fill="#101214" stroke="#000"/>`)
	digits := fmt.Sprintf("%08.0f", max(t.TotalBurn, 0))
	if len(digits) > 8 {
		digits = digits[len(digits)-8:] // a real drum rolls over too
	}
	const cell = 17.0
	left := 100 - cell*4
	for i, d := range digits {
		if i > 0 {
			x := left + cell*float64(i)
			fmt.Fprintf(&b, `<line x1="%s" y1="140" x2="%s" y2="170" stroke="#2a2e33" stroke-width="1"/>`, f(x), f(x))
		}
		fmt.Fprintf(&b, `<text x="%s" y="161" text-anchor="middle" class="tot-digit">%c</text>`, f(left+cell*float64(i)+cell/2), d)
	}

	b.WriteString(`</g></svg>`)
	return b.String()
}
