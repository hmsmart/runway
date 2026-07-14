package charts

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// topCategories sorts categories by their 14-day rate and keeps the first
// limit of them. Uncategorized spend is merged into a single Other entry up
// front, so a chart can never show two "other" rows: when fold is true the
// tail of named categories collapses into that same entry (part-to-whole
// charts keep their total); when false, Other competes by size like any
// category. EMAs are linear, so summing rates is exact.
func topCategories(cats []Category, limit int, fold bool) []Category {
	var named []Category
	other := Category{Name: OtherName}
	hasOther := false
	merge := func(c Category) {
		other.E14 += c.E14
		other.E28 += c.E28
		other.E84 += c.E84
		other.Recent = other.Recent || c.Recent
		hasOther = true
	}
	for _, c := range cats {
		if c.Name == "" || c.Name == OtherName {
			merge(c)
			continue
		}
		named = append(named, c)
	}
	sort.SliceStable(named, func(i, j int) bool { return named[i].E14 > named[j].E14 })

	if fold {
		if len(named) > limit {
			for _, c := range named[limit:] {
				merge(c)
			}
			named = named[:limit:limit]
		}
		if hasOther {
			named = append(named, other)
		}
		return named
	}
	if hasOther {
		named = append(named, other)
		sort.SliceStable(named, func(i, j int) bool { return named[i].E14 > named[j].E14 })
	}
	if len(named) > limit {
		named = named[:limit]
	}
	return named
}

// barLabel renders a bar-tip value: whole dollars once the rate has size,
// cents below $10 so a small rate never rounds to a meaningless "$0".
func barLabel(v float64) string {
	if v >= 9.5 {
		return fmtWhole(v)
	}
	return fmtCents(v)
}

func (c Category) ema(v View) float64 {
	switch v {
	case ViewEMA28:
		return c.E28
	case ViewEMA84:
		return c.E84
	default:
		return c.E14
	}
}

// emaLegend draws the shared three-key legend row the bars and radar charts
// open with, returning nothing but writing into b.
func emaLegend(b *strings.Builder, x, y float64) {
	for _, s := range emaSlots {
		fmt.Fprintf(b, `<line x1="%s" y1="%s" x2="%s" y2="%s" stroke="currentColor" stroke-width="2.5" class="%s"/>`,
			f(x), f(y-4), f(x+14), f(y-4), s.class)
		fmt.Fprintf(b, `<text x="%s" y="%s" font-size="11" class="muted">%s</text>`, f(x+19), f(y), s.label)
		x += 19 + float64(len(s.label))*6.5 + 16
	}
}

// CategoryBars renders the top spend categories as grouped horizontal bars,
// one bar per EMA horizon. Categories are the row labels; color carries only
// the horizon, so it matches the trend lines and donut buttons everywhere.
func CategoryBars(cats []Category) string {
	const (
		w, h     = 400, 400
		left     = 14
		right    = 52 // room for tip labels
		top      = 44
		groupH   = 57
		barH     = 9
		barGap   = 2
		maxShown = 6
	)
	shown := topCategories(cats, maxShown-1, true)

	var b strings.Builder
	svgOpen(&b, w, h, "spending rate by category", "")
	emaLegend(&b, left, 20)

	if len(shown) == 0 {
		fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="12" class="muted" text-anchor="middle">no spending history yet</text>`, w/2, h/2)
		b.WriteString("</svg>")
		return b.String()
	}

	maxV := 0.0
	for _, c := range shown {
		maxV = max(maxV, c.E14, c.E28, c.E84)
	}
	xMax := niceMax(maxV)
	scale := float64(w-left-right) / xMax

	for gi, c := range shown {
		gy := top + gi*groupH
		fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="11">%s</text>`, left, gy, esc(catLabel(c.Name)))
		for si, v := range [3]float64{c.E14, c.E28, c.E84} {
			s := emaSlots[si]
			y := gy + 6 + si*(barH+barGap)
			bw := v * scale
			r := min(4, bw/2)
			fmt.Fprintf(&b, `<path fill="currentColor" class="%s" d="M%d %dH%sQ%s %d %s %sV%sQ%s %d %s %dH%dZ"><title>%s %s — %s/day</title></path>`,
				s.class, left, y,
				f(float64(left)+bw-r), f(float64(left)+bw), y, f(float64(left)+bw), f(float64(y)+r),
				f(float64(y+barH)-r), f(float64(left)+bw), y+barH, f(float64(left)+bw-r), y+barH, left,
				esc(catLabel(c.Name)), s.label, esc(fmtCents(v)))
			fmt.Fprintf(&b, `<text x="%s" y="%d" font-size="10" class="muted">%s</text>`,
				f(float64(left)+bw+5), y+barH-1, esc(barLabel(v)))
		}
	}
	// Baseline on the left edge of the bars.
	fmt.Fprintf(&b, `<line x1="%d" y1="%d" x2="%d" y2="%d" class="axis"/>`,
		left-1, top-6, left-1, top+len(shown)*groupH-14)

	b.WriteString("</svg>")
	return b.String()
}

// CategoryDonut renders one EMA horizon's category shares as a donut, with
// pill links across the top to flip between horizons. Each pill wears its
// horizon's line color; the SVG navigates its own browsing context, so the
// chart swaps in place when embedded via <object>.
func CategoryDonut(cats []Category, view View) string {
	const (
		w, h   = 400, 400
		cx, cy = 200.0, 176.0
		rOut   = 90.0
		rIn    = 58.0
	)
	shown := topCategories(cats, 5, true)

	var css strings.Builder
	for _, c := range shown {
		css.WriteString(catColorRule(c.Name))
	}
	var b strings.Builder
	svgOpen(&b, w, h, "category share of daily rate", css.String())

	// Horizon pills. The selected one is washed and outlined in its color;
	// text stays in ink so it reads on any theme.
	const pillW, pillH, pillGap = 64.0, 24.0, 9.0
	px := (w - 3*pillW - 2*pillGap) / 2
	for _, s := range emaSlots {
		sel := s.view == view
		style := `stroke="currentColor" stroke-width="1" opacity="0.55" fill="none"`
		if sel {
			style = `stroke="currentColor" stroke-width="2" fill="currentColor" fill-opacity="0.16"`
		}
		fmt.Fprintf(&b, `<a href="?view=%s"><rect x="%s" y="12" width="%s" height="%s" rx="12" class="%s" %s/><text x="%s" y="28" font-size="11" text-anchor="middle">%s</text></a>`,
			s.view, f(px), f(pillW), f(pillH), s.class, style, f(px+pillW/2), s.label)
		px += pillW + pillGap
	}

	total := 0.0
	for _, c := range shown {
		total += c.ema(view)
	}
	if total <= 0 {
		fmt.Fprintf(&b, `<text x="%d" y="%s" font-size="12" class="muted" text-anchor="middle">no spending history yet</text>`, w/2, f(cy))
		b.WriteString("</svg>")
		return b.String()
	}

	// Slices, separated by a real ~2px gap (pad angle) rather than a stroke.
	pad := 2.0 / rOut
	angle := -math.Pi / 2
	for _, c := range shown {
		v := c.ema(view)
		sweep := v / total * 2 * math.Pi
		if sweep <= 0 {
			continue
		}
		a0, a1 := angle+pad/2, angle+sweep-pad/2
		angle += sweep
		if a1 <= a0 { // sliver thinner than the gap: keep a hairline slice
			a0, a1 = angle-sweep/2-0.004, angle-sweep/2+0.004
		}
		large := 0
		if a1-a0 > math.Pi {
			large = 1
		}
		x0o, y0o := cx+rOut*math.Cos(a0), cy+rOut*math.Sin(a0)
		x1o, y1o := cx+rOut*math.Cos(a1), cy+rOut*math.Sin(a1)
		x0i, y0i := cx+rIn*math.Cos(a1), cy+rIn*math.Sin(a1)
		x1i, y1i := cx+rIn*math.Cos(a0), cy+rIn*math.Sin(a0)
		fmt.Fprintf(&b, `<path fill="currentColor" class="cat-%s" d="M%s %sA%s %s 0 %d 1 %s %sL%s %sA%s %s 0 %d 0 %s %sZ"><title>%s — %s/day (%.0f%%)</title></path>`,
			catSlug(c.Name),
			f(x0o), f(y0o), f(rOut), f(rOut), large, f(x1o), f(y1o),
			f(x0i), f(y0i), f(rIn), f(rIn), large, f(x1i), f(y1i),
			esc(catLabel(c.Name)), esc(fmtCents(v)), v/total*100)
	}

	// Center hero: the horizon's total daily rate.
	fmt.Fprintf(&b, `<text x="%s" y="%s" font-size="26" font-weight="600" text-anchor="middle">%s</text>`,
		f(cx), f(cy+2), esc(fmtWhole(total)))
	fmt.Fprintf(&b, `<text x="%s" y="%s" font-size="10.5" class="muted" text-anchor="middle">per day · %s</text>`,
		f(cx), f(cy+20), ParseView(string(view)))

	// Legend, two columns under the ring.
	const legTop, rowH = 296, 26
	for i, c := range shown {
		x := 34 + (i%2)*186
		y := legTop + (i/2)*rowH
		fmt.Fprintf(&b, `<rect x="%d" y="%d" width="10" height="10" rx="3" fill="currentColor" class="cat-%s"/>`, x, y, catSlug(c.Name))
		fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="11">%s</text>`, x+16, y+9, esc(catLabel(c.Name)))
		fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="11" class="muted" text-anchor="end">%.0f%%</text>`, x+170, y+9, c.ema(view)/total*100)
	}

	b.WriteString("</svg>")
	return b.String()
}

// CategoryRadar renders the top categories as radar axes with one polygon
// per EMA horizon, so shape changes show where the short-term mix is
// drifting from the long-term one. Only categories with spending inside the
// shortest window qualify for an axis: a category that's merely decaying
// would pin the 14-day polygon to the center and read as noise.
func CategoryRadar(cats []Category) string {
	const (
		w, h     = 400, 400
		cx, cy   = 200.0, 220.0
		radius   = 118.0
		maxAxes  = 6
		labelPad = 14.0
	)
	var active []Category
	for _, c := range cats {
		if c.Recent {
			active = append(active, c)
		}
	}
	shown := topCategories(active, maxAxes, false)

	var b strings.Builder
	svgOpen(&b, w, h, "category rates by horizon", "")
	emaLegend(&b, 14, 20)

	if len(shown) < 3 {
		fmt.Fprintf(&b, `<text x="%d" y="%s" font-size="12" class="muted" text-anchor="middle">need recent spending in 3+ categories</text>`, w/2, f(cy))
		b.WriteString("</svg>")
		return b.String()
	}

	maxV := 0.0
	for _, c := range shown {
		maxV = max(maxV, c.E14, c.E28, c.E84)
	}
	vMax := niceMax(maxV)
	n := len(shown)
	point := func(axis int, frac float64) (float64, float64) {
		a := -math.Pi/2 + 2*math.Pi*float64(axis)/float64(n)
		return cx + radius*frac*math.Cos(a), cy + radius*frac*math.Sin(a)
	}

	// Rings and spokes, recessive.
	for _, frac := range []float64{0.25, 0.5, 0.75, 1} {
		var pts []string
		for i := range shown {
			x, y := point(i, frac)
			pts = append(pts, f(x)+","+f(y))
		}
		fmt.Fprintf(&b, `<polygon points="%s" fill="none" class="grid"/>`, strings.Join(pts, " "))
	}
	for i := range shown {
		x, y := point(i, 1)
		fmt.Fprintf(&b, `<line x1="%s" y1="%s" x2="%s" y2="%s" class="grid"/>`, f(cx), f(cy), f(x), f(y))
	}
	// Ring values along the top spoke, so the rings read as a scale rather
	// than decoration. The unit rides only the outer ring's label.
	for _, frac := range []float64{0.25, 0.5, 0.75} {
		fmt.Fprintf(&b, `<text x="%s" y="%s" font-size="9" class="muted">%s</text>`,
			f(cx+6), f(cy-radius*frac+10), esc(barLabel(vMax*frac)))
	}
	fmt.Fprintf(&b, `<text x="%s" y="%s" font-size="9" class="muted">%s/day</text>`,
		f(cx+6), f(cy-radius+10), esc(barLabel(vMax)))

	// Axis labels, anchored away from the plot by angle; long names wrap on
	// their last space so they stay clear of the rings.
	for i, c := range shown {
		a := -math.Pi/2 + 2*math.Pi*float64(i)/float64(n)
		x := cx + (radius+labelPad)*math.Cos(a)
		y := cy + (radius+labelPad)*math.Sin(a)
		anchor := "middle"
		if math.Cos(a) > 0.3 {
			anchor = "start"
		} else if math.Cos(a) < -0.3 {
			anchor = "end"
		}
		label := catLabel(c.Name)
		lines := []string{label}
		if len(label) > 13 {
			if cut := strings.LastIndex(label, " "); cut > 0 {
				lines = []string{label[:cut], label[cut+1:]}
			}
		}
		if math.Sin(a) < -0.3 && len(lines) == 2 {
			y -= 12 // wrapped label above the plot needs room for both lines
		}
		fmt.Fprintf(&b, `<text x="%s" y="%s" font-size="10.5" class="muted" text-anchor="%s">`, f(x), f(y+3), anchor)
		for li, ln := range lines {
			fmt.Fprintf(&b, `<tspan x="%s" dy="%d">%s</tspan>`, f(x), li*12, esc(ln))
		}
		b.WriteString(`</text>`)
	}

	// Polygons, longest horizon first so the freshest sits on top.
	for si := len(emaSlots) - 1; si >= 0; si-- {
		s := emaSlots[si]
		var pts []string
		for i, c := range shown {
			v := [3]float64{c.E14, c.E28, c.E84}[si]
			x, y := point(i, min(v/vMax, 1))
			pts = append(pts, f(x)+","+f(y))
		}
		fmt.Fprintf(&b, `<polygon points="%s" fill="currentColor" fill-opacity="0.1" stroke="currentColor" stroke-width="2" stroke-linejoin="round" class="%s"><title>%s</title></polygon>`,
			strings.Join(pts, " "), s.class, s.label)
	}

	b.WriteString("</svg>")
	return b.String()
}
