package charts

import (
	"fmt"
	"strings"
)

// EGTPanel renders the per-category engine-temp panel as a 200×400 (1×2 tall)
// dark panel. Each category gets a horizontal EGT strip with green/amber/red
// zones. The needle shows the 14-day rate relative to the 84-day baseline.
//
// A TOTAL row at the bottom sums all categories (not just the visible top 8)
// so the pilot can read overall spend-rate drift at a glance.
//
// Zone thresholds:
//   - Green:  ≤1× baseline (nominal)
//   - Amber:  >1× to 1.5× baseline (warm)
//   - Red:    >1.5× baseline (hot)
func EGTPanel(cats []Category) string {
	const (
		w, h     = 200, 400
		maxShown = 8
		stripL   = 16.0
		stripR   = 184.0
		stripH   = 12.0
		rowH     = 34.0
		top      = 64.0
	)
	shown := topCategories(cats, maxShown, false)
	stripW := stripR - stripL

	// Compute totals across ALL categories (not just shown).
	var totalE14, totalE84 float64
	for _, c := range cats {
		totalE14 += c.E14
		totalE84 += c.E84
	}

	var css strings.Builder
	for _, c := range shown {
		css.WriteString(catColorRule(c.Name))
	}

	var b strings.Builder
	svgOpen(&b, w, h, "engine temps — category spend rates vs baseline", css.String()+`
.egt-panel{fill:#0f1318;stroke:var(--chart-axis,#c6ccd4);stroke-width:1.5;rx:10}
.egt-title{font-size:9px;letter-spacing:2.5px}
.egt-label{font-family:'B612 Mono',var(--chart-font,system-ui,sans-serif);font-size:8px;letter-spacing:0.3px}
.egt-val{font-family:'B612 Mono',var(--chart-font,system-ui,sans-serif);font-size:9px;font-weight:700}
.egt-strip{fill:#14181e;stroke:#262b33;stroke-width:0.5}
.egt-zone-g{fill:rgba(99,153,34,0.15)}
.egt-zone-a{fill:rgba(217,145,20,0.15)}
.egt-zone-r{fill:rgba(226,75,74,0.1)}
.egt-baseline{stroke:var(--chart-ema14,#6da7ec);stroke-width:0.75;opacity:0.6}
.egt-divider{stroke:#262b33;stroke-width:1}
.egt-total-label{font-family:'B612 Mono',var(--chart-font,system-ui,sans-serif);font-size:9px;font-weight:700;letter-spacing:1px}
.egt-footer{font-family:'B612 Mono',var(--chart-font,system-ui,sans-serif);font-size:7px;letter-spacing:1px}
`)

	b.WriteString(`<g>`)
	b.WriteString(`<rect x="8" y="8" width="184" height="384" class="egt-panel"/>`)
	b.WriteString(`<text x="100" y="26" text-anchor="middle" class="egt-title" style="fill:#5a6270">EGT · CATEGORY</text>`)

	// Legend row.
	b.WriteString(`<line x1="22" y1="40" x2="32" y2="40" stroke="var(--chart-ema14,#6da7ec)" stroke-width="1.5"/>`)
	b.WriteString(`<text x="35" y="43" font-size="7" style="fill:#5a6270">84d baseline</text>`)
	fmt.Fprintf(&b, `<circle cx="100" cy="40" r="2.5" fill="%s"/>`, gaugeGreen)
	b.WriteString(`<text x="106" y="43" font-size="7" style="fill:#5a6270">&lt;1×</text>`)
	fmt.Fprintf(&b, `<circle cx="126" cy="40" r="2.5" fill="%s"/>`, gaugeAmber)
	b.WriteString(`<text x="132" y="43" font-size="7" style="fill:#5a6270">&lt;1.5×</text>`)
	fmt.Fprintf(&b, `<circle cx="158" cy="40" r="2.5" fill="%s"/>`, gaugeRed)
	b.WriteString(`<text x="164" y="43" font-size="7" style="fill:#5a6270">&gt;1.5×</text>`)

	if len(shown) == 0 {
		b.WriteString(`<text x="100" y="200" font-size="11" text-anchor="middle" class="muted">no spending history yet</text>`)
		b.WriteString(`</g></svg>`)
		return b.String()
	}

	// ── TOTAL row (first, at the top) ─────────────────────────────
	totalRatio := egtRatio(Category{E14: totalE14, E84: totalE84})
	totalColor := egtColor(totalRatio)

	fmt.Fprintf(&b, `<text x="%s" y="%s" class="egt-total-label" style="fill:#8891a0">TOTAL</text>`,
		f(stripL), f(top+10))
	fmt.Fprintf(&b, `<text x="%s" y="%s" text-anchor="end" class="egt-val" style="fill:%s;font-size:10px">%s</text>`,
		f(stripR), f(top+10), totalColor, esc(egtRatioLabel(totalRatio)))

	tsy := top + 14
	fmt.Fprintf(&b, `<rect x="%s" y="%s" width="%s" height="%s" rx="2" class="egt-strip"/>`,
		f(stripL), f(tsy), f(stripW), f(stripH))
	midX := stripL + stripW*0.5
	fmt.Fprintf(&b, `<rect x="%s" y="%s" width="%s" height="%s" rx="2" class="egt-zone-g"/>`,
		f(stripL), f(tsy), f(stripW*0.5), f(stripH))
	fmt.Fprintf(&b, `<rect x="%s" y="%s" width="%s" height="%s" class="egt-zone-a"/>`,
		f(midX), f(tsy), f(stripW*0.25), f(stripH))
	fmt.Fprintf(&b, `<rect x="%s" y="%s" width="%s" height="%s" class="egt-zone-r"/>`,
		f(midX+stripW*0.25), f(tsy), f(stripW*0.25), f(stripH))
	fmt.Fprintf(&b, `<line x1="%s" y1="%s" x2="%s" y2="%s" class="egt-baseline"/>`,
		f(midX), f(tsy), f(midX), f(tsy+stripH))
	needleX := stripL + stripW*clamp(totalRatio/2, 0, 1)
	fmt.Fprintf(&b, `<rect x="%s" y="%s" width="3" height="%s" rx="1.5" fill="%s"/>`,
		f(needleX-1.5), f(tsy+1.5), f(stripH-3), totalColor)

	fmt.Fprintf(&b, `<line x1="24" y1="%s" x2="176" y2="%s" class="egt-divider"/>`,
		f(tsy+stripH+6), f(tsy+stripH+6))

	// ── Per-category rows ────────────────────────────────────────
	catTop := tsy + stripH + 12
	for i, c := range shown {
		y := catTop + rowH*float64(i)
		sy := y + 12 // strip top

		// Category dot + label.
		fmt.Fprintf(&b, `<rect x="%s" y="%s" width="7" height="7" rx="2" fill="currentColor" class="cat-%s"/>`,
			f(stripL), f(y), catSlug(c.Name))
		fmt.Fprintf(&b, `<text x="%s" y="%s" class="egt-label" style="fill:#8891a0">%s</text>`,
			f(stripL+12), f(y+7), esc(strings.ToUpper(catLabel(c.Name))))

		// Ratio value.
		ratio := egtRatio(c)
		color := egtColor(ratio)
		fmt.Fprintf(&b, `<text x="%s" y="%s" text-anchor="end" class="egt-val" style="fill:%s">%s</text>`,
			f(stripR), f(y+7), color, esc(egtRatioLabel(ratio)))

		// Strip background.
		fmt.Fprintf(&b, `<rect x="%s" y="%s" width="%s" height="%s" rx="2" class="egt-strip"/>`,
			f(stripL), f(sy), f(stripW), f(stripH))

		// Zone fills.
		midX := stripL + stripW*0.5
		fmt.Fprintf(&b, `<rect x="%s" y="%s" width="%s" height="%s" rx="2" class="egt-zone-g"/>`,
			f(stripL), f(sy), f(stripW*0.5), f(stripH))
		fmt.Fprintf(&b, `<rect x="%s" y="%s" width="%s" height="%s" class="egt-zone-a"/>`,
			f(midX), f(sy), f(stripW*0.25), f(stripH))
		fmt.Fprintf(&b, `<rect x="%s" y="%s" width="%s" height="%s" class="egt-zone-r"/>`,
			f(midX+stripW*0.25), f(sy), f(stripW*0.25), f(stripH))

		// Baseline marker.
		fmt.Fprintf(&b, `<line x1="%s" y1="%s" x2="%s" y2="%s" class="egt-baseline"/>`,
			f(midX), f(sy), f(midX), f(sy+stripH))

		// Needle.
		needleX := stripL + stripW*clamp(ratio/2, 0, 1)
		fmt.Fprintf(&b, `<rect x="%s" y="%s" width="3" height="%s" rx="1.5" fill="%s"/>`,
			f(needleX-1.5), f(sy+1.5), f(stripH-3), color)
	}

	// Footer.
	b.WriteString(`<text x="100" y="386" text-anchor="middle" class="egt-footer" style="fill:#5a6270">14D RATE / 84D BASELINE</text>`)

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
