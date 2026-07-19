package charts

import (
	"fmt"
	"strings"
)

// CDIState is the course-deviation instrument's input: how far cumulative
// month-to-date spend has drifted from the budget's planned pace.
type CDIState struct {
	// DevDays is the deviation in days of daily allowance: positive means
	// over budget (needle right), negative under (needle left). One dot on
	// the scale is one day's allowance; the needle pegs past five.
	DevDays float64

	// DevDollars is the same deviation in dollars, for the readout.
	DevDollars float64

	// HasBudget is false when the user has no monthly budget set; the
	// instrument shows a NAV flag instead of a needle.
	HasBudget bool
}

// CDI renders the budget course-deviation indicator: a centered dot scale
// with a vertical needle showing accumulated month-to-date drift from the
// planned cumulative spend. The ADI shows today's rate; this shows the damage
// (or savings) banked so far this month.
func CDI(state CDIState) string {
	const (
		cx, cy   = 100.0, 100.0
		dotStep  = 13.0 // px per day of allowance
		maxDefl  = 5.3  // needle clamp, in dots
		needleT  = 58.0 // needle top y
		needleB  = 142.0
	)

	var b strings.Builder
	svgOpen(&b, 200, 200, "budget course deviation indicator", `
.cdi .readout-window{fill:var(--chart-heat-0,#e9ecef);stroke:var(--chart-axis,#c6ccd4);stroke-width:1.5}
.cdi .readout{fill:var(--chart-ink,#33383f);font-family:'B612 Mono',var(--chart-font,system-ui,sans-serif);font-size:11px;font-weight:700}
.cdi .needle{stroke:#ff8c00;stroke-width:3.5;stroke-linecap:round}
`)
	b.WriteString(`<g class="cdi">`)
	bezel(&b, cx, cy)

	b.WriteString(`<text x="100" y="52" text-anchor="middle" font-size="10" font-weight="700" class="muted" letter-spacing="0.14em">BUDGET</text>`)

	// Dot scale: five dots each side, one day's allowance apart, around a
	// center ring.
	fmt.Fprintf(&b, `<circle cx="%s" cy="%s" r="5.5" fill="none" stroke="var(--chart-ink,#33383f)" stroke-width="1.5"/>`, f(cx), f(cy))
	for i := 1; i <= 5; i++ {
		dx := dotStep * float64(i)
		fmt.Fprintf(&b, `<circle cx="%s" cy="%s" r="2.5" fill="var(--chart-muted,#6a7280)"/>`, f(cx-dx), f(cy))
		fmt.Fprintf(&b, `<circle cx="%s" cy="%s" r="2.5" fill="var(--chart-muted,#6a7280)"/>`, f(cx+dx), f(cy))
	}
	// style= (not the fill attribute) because svgOpen's text{fill:ink} rule
	// overrides presentation attributes.
	fmt.Fprintf(&b, `<text x="42" y="130" text-anchor="middle" font-size="7.5" font-weight="700" style="fill:%s" letter-spacing="0.1em">UNDER</text>`, gaugeGreen)
	fmt.Fprintf(&b, `<text x="158" y="130" text-anchor="middle" font-size="7.5" font-weight="700" style="fill:%s" letter-spacing="0.1em">OVER</text>`, gaugeRed)
	b.WriteString(`<text x="100" y="76" text-anchor="middle" font-size="8" class="muted" letter-spacing="0.08em">1 DOT = 1 DAY</text>`)

	if !state.HasBudget {
		// NAV flag: no budget signal to track.
		fmt.Fprintf(&b, `<rect x="76" y="86" width="48" height="26" rx="4" fill="%s"/>`, gaugeRed)
		b.WriteString(`<text x="100" y="103" text-anchor="middle" font-size="12" font-weight="700" fill="#fff" letter-spacing="0.14em">NAV</text>`)
		b.WriteString(`<text x="100" y="168" text-anchor="middle" font-size="9" class="muted">no monthly budget set</text>`)
		b.WriteString(`</g></svg>`)
		return b.String()
	}

	x := cx + dotStep*clamp(state.DevDays, -maxDefl, maxDefl)
	fmt.Fprintf(&b, `<line x1="%s" y1="%s" x2="%s" y2="%s" class="needle"/>`, f(x), f(needleT), f(x), f(needleB))

	b.WriteString(`<rect x="48" y="152" width="104" height="24" rx="4" class="readout-window"/>`)
	fmt.Fprintf(&b, `<text x="100" y="168" text-anchor="middle" class="readout">%s</text>`, esc(cdiReadout(state)))

	b.WriteString(`</g></svg>`)
	return b.String()
}

// cdiReadout renders the deviation for the window: "+1.8d · +$74" means the
// month is running 1.8 allowance-days ($74) over plan.
func cdiReadout(s CDIState) string {
	sign := "+"
	if s.DevDays < 0 {
		sign = "-"
	}
	days := s.DevDays
	dollars := s.DevDollars
	if days < 0 {
		days, dollars = -days, -dollars
	}
	return fmt.Sprintf("%s%.1fd · %s$%s", sign, days, sign, comma(fmt.Sprintf("%.0f", dollars)))
}
