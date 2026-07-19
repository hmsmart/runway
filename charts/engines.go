package charts

import (
	"fmt"
	"strings"
)

// Engines renders the per-category engine-temp panel: one row per category
// with its identity dot, current rate, a status lamp, and an EQ-style bar
// pair — the 14-day rate over its 84-day baseline on a shared scale, so
// "running hot" is literally the top bar outgrowing the bottom one. The lamp
// alone carries the judgment (green nominal, amber past baseline, red past
// 1.25×); category colors are identity only.
func Engines(cats []Category) string {
	const (
		w, h    = 800, 170
		colW    = 400.0
		rowH    = 44.0
		top     = 40.0
		left    = 24.0
		barMaxW = 330.0
	)
	shown := topCategories(cats, 6, false)

	var css strings.Builder
	for _, c := range shown {
		css.WriteString(catColorRule(c.Name))
	}
	var b strings.Builder
	svgOpen(&b, w, h, "engine temps — category spend rates vs baseline", css.String())

	if len(shown) == 0 {
		fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="12" class="muted" text-anchor="middle">no spending history yet</text>`, w/2, h/2)
		b.WriteString("</svg>")
		return b.String()
	}

	// Shared scale across every bar, so rows are comparable.
	maxV := 0.0
	for _, c := range shown {
		maxV = max(maxV, c.E14, c.E84)
	}
	xMax := niceMax(maxV)

	// Bar legend, horizon colors matching every other chart.
	b.WriteString(`<line x1="24" y1="16" x2="38" y2="16" stroke="currentColor" stroke-width="4" class="ema14"/>`)
	b.WriteString(`<text x="43" y="19" font-size="9" class="muted">14d now</text>`)
	b.WriteString(`<line x1="94" y1="16" x2="108" y2="16" stroke="currentColor" stroke-width="4" class="ema84"/>`)
	b.WriteString(`<text x="113" y="19" font-size="9" class="muted">84d baseline</text>`)

	for i, c := range shown {
		x := left + colW*float64(i/3)
		y := top + rowH*float64(i%3)
		b.WriteString(`<g>`)
		fmt.Fprintf(&b, `<title>%s — %s/day now vs %s/day baseline</title>`,
			esc(catLabel(c.Name)), esc(fmtCents(c.E14)), esc(fmtCents(c.E84)))

		// Identity dot + name.
		fmt.Fprintf(&b, `<rect x="%s" y="%s" width="9" height="9" rx="3" fill="currentColor" class="cat-%s"/>`, f(x), f(y-8), catSlug(c.Name))
		fmt.Fprintf(&b, `<text x="%s" y="%s" font-size="11">%s</text>`, f(x+15), f(y), esc(catLabel(c.Name)))

		// Rate readout and status lamp.
		fmt.Fprintf(&b, `<text x="%s" y="%s" font-size="10" text-anchor="end" class="muted">%s/d</text>`, f(x+barMaxW), f(y), esc(barLabel(c.E14)))
		fmt.Fprintf(&b, `<circle cx="%s" cy="%s" r="6" fill="%s" stroke="var(--chart-axis,#c6ccd4)" stroke-width="1"/>`, f(x+barMaxW+18), f(y-3.5), engLampColor(c))

		// The EQ pair: now over baseline.
		fmt.Fprintf(&b, `<rect x="%s" y="%s" width="%s" height="5" rx="2.5" fill="currentColor" class="ema14"/>`, f(x), f(y+7), f(max(c.E14/xMax*barMaxW, 2)))
		fmt.Fprintf(&b, `<rect x="%s" y="%s" width="%s" height="5" rx="2.5" fill="currentColor" class="ema84"/>`, f(x), f(y+15), f(max(c.E84/xMax*barMaxW, 2)))
		b.WriteString(`</g>`)
	}

	b.WriteString("</svg>")
	return b.String()
}

// engLampColor grades a category's 14-day rate against its own 84-day
// baseline: green at or under it, amber up to 1.25×, red beyond. A category
// too new for a baseline reads hot when it's spending and nominal when idle.
func engLampColor(c Category) string {
	ratio := c.E14 / c.E84
	if c.E84 < 0.01 {
		if c.E14 < 0.01 {
			return gaugeGreen
		}
		return gaugeRed
	}
	switch {
	case ratio > 1.25:
		return gaugeRed
	case ratio > 1:
		return gaugeAmber
	default:
		return gaugeGreen
	}
}
