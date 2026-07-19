package charts

import (
	"fmt"
	"strings"
)

// Engines renders the per-category engine-temp panel as a 200×200 SVG rect
// instrument matching the DME/totalizer style. Each category gets a
// horizontal EGT strip with green/amber/red zones. The needle position shows
// the 14-day rate relative to the 84-day baseline: left of center is cooler
// than baseline, right is hotter. The baseline marker sits at center.
//
// Zone thresholds:
//   - Green:  ≤1× baseline (nominal)
//   - Amber:  >1× to 1.5× baseline (warm)
//   - Red:    >1.5× baseline (hot)
//
// Shows top 5 categories by 14-day rate.
func Engines(cats []Category) string {
	const (
		w, h     = 200, 200
		maxShown = 5
		stripL   = 16.0  // strip left edge
		stripR   = 184.0 // strip right edge
		stripH   = 10.0
		rowH     = 26.0
		top      = 48.0 // y of first row label
	)
	shown := topCategories(cats, maxShown, false)
	stripW := stripR - stripL

	var css strings.Builder
	for _, c := range shown {
		css.WriteString(catColorRule(c.Name))
	}
	var b strings.Builder
	svgOpen(&b, w, h, "engine temps — category spend rates vs baseline", css.String()+`
.egt-panel{fill:#0f1318;stroke:var(--chart-axis,#c6ccd4);stroke-width:1.5;rx:10}
.egt-title{font-size:9px;letter-spacing:2.5px}
.egt-label{font-family:'B612 Mono',var(--chart-font,system-ui,sans-serif);font-size:7px;letter-spacing:0.3px}
.egt-val{font-family:'B612 Mono',var(--chart-font,system-ui,sans-serif);font-size:8px;font-weight:700}
.egt-strip{fill:#14181e;stroke:#262b33;stroke-width:0.5}
.egt-zone-g{fill:rgba(99,153,34,0.15)}
.egt-zone-a{fill:rgba(217,145,20,0.15)}
.egt-zone-r{fill:rgba(226,75,74,0.1)}
.egt-baseline{stroke:var(--chart-ema14,#6da7ec);stroke-width:0.75;opacity:0.6}
.egt-footer{font-family:'B612 Mono',var(--chart-font,system-ui,sans-serif);font-size:7px;letter-spacing:1px}
`)

	b.WriteString(`<g>`)
	b.WriteString(`<rect x="8" y="8" width="184" height="184" class="egt-panel"/>`)
	b.WriteString(`<text x="100" y="26" text-anchor="middle" class="egt-title" style="fill:#5a6270">EGT · CATEGORY</text>`)

	// Legend row.
	b.WriteString(`<line x1="22" y1="36" x2="30" y2="36" stroke="var(--chart-ema14,#6da7ec)" stroke-width="1.5"/>`)
	b.WriteString(`<text x="33" y="39" font-size="6" style="fill:#5a6270">84d</text>`)
	fmt.Fprintf(&b, `<circle cx="58" cy="36" r="2" fill="%s"/>`, gaugeGreen)
	b.WriteString(`<text x="63" y="39" font-size="6" style="fill:#5a6270">&lt;1×</text>`)
	fmt.Fprintf(&b, `<circle cx="84" cy="36" r="2" fill="%s"/>`, gaugeAmber)
	b.WriteString(`<text x="89" y="39" font-size="6" style="fill:#5a6270">&lt;1.5×</text>`)
	fmt.Fprintf(&b, `<circle cx="114" cy="36" r="2" fill="%s"/>`, gaugeRed)
	b.WriteString(`<text x="119" y="39" font-size="6" style="fill:#5a6270">&gt;1.5×</text>`)

	if len(shown) == 0 {
		b.WriteString(`<text x="100" y="120" font-size="11" text-anchor="middle" class="muted">no spending history yet</text>`)
		b.WriteString(`</g></svg>`)
		return b.String()
	}

	for i, c := range shown {
		y := top + rowH*float64(i)
		sy := y + 9 // strip top

		// Category dot + label.
		fmt.Fprintf(&b, `<rect x="%s" y="%s" width="6" height="6" rx="2" fill="currentColor" class="cat-%s"/>`,
			f(stripL), f(y-2), catSlug(c.Name))
		fmt.Fprintf(&b, `<text x="%s" y="%s" class="egt-label" style="fill:#8891a0">%s</text>`,
			f(stripL+10), f(y+4), esc(strings.ToUpper(catLabel(c.Name))))

		// Ratio value, colored by zone.
		ratio := egtRatio(c)
		color := egtColor(ratio)
		fmt.Fprintf(&b, `<text x="%s" y="%s" text-anchor="end" class="egt-val" style="fill:%s">%s</text>`,
			f(stripR), f(y+4), color, esc(egtRatioLabel(ratio)))

		// Strip background.
		fmt.Fprintf(&b, `<rect x="%s" y="%s" width="%s" height="%s" rx="2" class="egt-strip"/>`,
			f(stripL), f(sy), f(stripW), f(stripH))

		// Zone fills: green left half, amber next quarter, red last quarter.
		// These represent 0-1×, 1-1.5×, 1.5-2× of baseline.
		midX := stripL + stripW*0.5
		fmt.Fprintf(&b, `<rect x="%s" y="%s" width="%s" height="%s" rx="2" class="egt-zone-g"/>`,
			f(stripL), f(sy), f(stripW*0.5), f(stripH))
		fmt.Fprintf(&b, `<rect x="%s" y="%s" width="%s" height="%s" class="egt-zone-a"/>`,
			f(midX), f(sy), f(stripW*0.25), f(stripH))
		fmt.Fprintf(&b, `<rect x="%s" y="%s" width="%s" height="%s" class="egt-zone-r"/>`,
			f(midX+stripW*0.25), f(sy), f(stripW*0.25), f(stripH))

		// Baseline marker at center.
		fmt.Fprintf(&b, `<line x1="%s" y1="%s" x2="%s" y2="%s" class="egt-baseline"/>`,
			f(midX), f(sy), f(midX), f(sy+stripH))

		// Needle: ratio maps onto the strip. 0× = left edge, 2× = right edge.
		// Clamp so the needle can't escape the strip.
		needleX := stripL + stripW*clamp(ratio/2, 0, 1)
		fmt.Fprintf(&b, `<rect x="%s" y="%s" width="2.5" height="%s" rx="1" fill="%s"/>`,
			f(needleX-1.25), f(sy+1), f(stripH-2), color)
	}

	// Footer.
	b.WriteString(`<text x="100" y="186" text-anchor="middle" class="egt-footer" style="fill:#5a6270">14D RATE / 84D BASELINE</text>`)

	b.WriteString(`</g></svg>`)
	return b.String()
}

// egtRatio computes the 14-day to 84-day ratio for a category. A category
// with no baseline (E84 near zero) reads as hot when spending, cold when
// idle — same logic as the old engLampColor.
func egtRatio(c Category) float64 {
	if c.E84 < 0.01 {
		if c.E14 < 0.01 {
			return 0
		}
		return 2.5 // peg hot
	}
	return c.E14 / c.E84
}

// egtColor returns the needle/label color for a given ratio.
func egtColor(ratio float64) string {
	switch {
	case ratio > 1.5:
		return gaugeRed
	case ratio > 1:
		return gaugeAmber
	default:
		return gaugeGreen
	}
}

// egtRatioLabel formats the ratio for display: "1.3×".
func egtRatioLabel(ratio float64) string {
	if ratio >= 10 {
		return fmt.Sprintf("%.0f×", ratio)
	}
	return fmt.Sprintf("%.1f×", ratio)
}
