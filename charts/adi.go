package charts

import (
	"fmt"
	"math"
	"strings"
)

// ADIState represents the combined attitude + course-deviation indicator.
type ADIState struct {
	Pitch      float64
	BurnMTD    string
	Ticks      []ADITick
	HasBudget  bool
	DevDays    float64
	DevDollars float64
	FuelWarn   bool
}

type ADITick struct {
	DY    float64
	Label string
	Sky   bool
}

func (t ADITick) Fill() string {
	if t.Sky {
		return "rgba(160,230,180,0.85)"
	}
	return "rgba(255,170,160,0.85)"
}

func ComputeADI(targetDaily, ema14, ema28, burnMTD float64, hasBudget bool, devDays, devDollars, fuelDays float64, daysLeft int) ADIState {
	if targetDaily <= 0 {
		targetDaily = 1
	}
	ratio := (targetDaily - ema14) / targetDaily
	pitch := clamp(ratio*55, -45, 45)

	fuelWarn := false
	if fuelDays > 0 && daysLeft > 0 && fuelDays < float64(daysLeft) {
		fuelWarn = true
		fuelDeficit := (float64(daysLeft) - fuelDays) / float64(daysLeft)
		pullDown := clamp(fuelDeficit*40, 0, 30)
		pitch = clamp(pitch-pullDown, -45, 45)
	}

	return ADIState{
		Pitch:      pitch,
		BurnMTD:    formatUSD(burnMTD),
		Ticks:      buildTicks(targetDaily, ema14, ema28),
		HasBudget:  hasBudget,
		DevDays:    devDays,
		DevDollars: devDollars,
		FuelWarn:   fuelWarn,
	}
}

func ADI(state ADIState) string {
	const (
		w, h    = 200, 200
		outerR  = 100
		innerR  = 94
		clipR   = 92
		dotStep = 13.0
		maxDefl = 5.0
	)

	var b strings.Builder
	svgOpen(&b, w, h, "spend attitude + course deviation indicator", `
.adi-bezel{fill:var(--chart-bar,#cdd3da)}
.adi-bezel-inner{stroke:var(--chart-heat-0,#e9ecef)}
.adi-bezel-ring{stroke:var(--chart-axis,#c6ccd4)}
.adi-sky{fill:#3a7bd5}
.adi-ground{fill:#8b6914}
.adi-readout{fill:rgba(0,0,0,0.78);stroke:rgba(255,255,255,0.35);stroke-width:0.75}
.adi-readout-label{font-family:'B612 Mono',var(--chart-font,system-ui,sans-serif);font-size:6px}
.adi-readout-val{font-family:'B612 Mono',var(--chart-font,system-ui,sans-serif);font-size:10px;font-weight:700}
`)

	b.WriteString(`<defs><clipPath id="adi-clip"><circle cx="100" cy="100" r="92"/></clipPath></defs>`)
	b.WriteString(`<g class="adi">`)
	fmt.Fprintf(&b, `<circle cx="100" cy="100" r="%d" class="adi-bezel"/>`, outerR)
	fmt.Fprintf(&b, `<circle cx="100" cy="100" r="%d" fill="none" class="adi-bezel-inner" stroke-width="2"/>`, innerR)

	// Sky / ground with pitch.
	fmt.Fprintf(&b, `<g clip-path="url(#adi-clip)"><g transform="translate(0,%s)">`, f(state.Pitch))
	b.WriteString(`<rect x="-100" y="-300" width="400" height="400" class="adi-sky"/>`)
	b.WriteString(`<rect x="-100" y="100" width="400" height="400" class="adi-ground"/>`)
	b.WriteString(`<line x1="-100" y1="100" x2="300" y2="100" stroke="#fff" stroke-width="2"/>`)
	b.WriteString(`<line x1="70" y1="80" x2="130" y2="80" stroke="rgba(255,255,255,0.5)" stroke-width="1"/>`)
	b.WriteString(`<line x1="75" y1="60" x2="125" y2="60" stroke="rgba(255,255,255,0.4)" stroke-width="1"/>`)
	b.WriteString(`<line x1="80" y1="40" x2="120" y2="40" stroke="rgba(255,255,255,0.3)" stroke-width="1"/>`)
	b.WriteString(`<line x1="70" y1="120" x2="130" y2="120" stroke="rgba(255,255,255,0.5)" stroke-width="1"/>`)
	b.WriteString(`<line x1="75" y1="140" x2="125" y2="140" stroke="rgba(255,255,255,0.4)" stroke-width="1"/>`)
	b.WriteString(`<line x1="80" y1="160" x2="120" y2="160" stroke="rgba(255,255,255,0.3)" stroke-width="1"/>`)
	for _, tick := range state.Ticks {
		if screenY := 100 + tick.DY + state.Pitch; screenY < 52 || screenY > 148 {
			continue
		}
		fmt.Fprintf(&b, `<text x="55" y="%s" text-anchor="end" font-family="'B612 Mono',monospace" font-size="9" font-weight="700" style="fill:%s">%s</text>`,
			f(100+tick.DY+4), esc(tick.Fill()), esc(tick.Label))
	}
	b.WriteString(`</g></g>`)

	// GS dots (right edge).
	gsX := 178.0
	fmt.Fprintf(&b, `<circle cx="%s" cy="100" r="3.5" fill="none" stroke="rgba(255,255,255,0.5)" stroke-width="1.2"/>`, f(gsX))
	for _, dy := range []float64{-14, -28, 14, 28} {
		fmt.Fprintf(&b, `<circle cx="%s" cy="%s" r="2" fill="rgba(255,255,255,0.35)"/>`, f(gsX), f(100+dy))
	}
	gsDY := clamp(-state.Pitch*0.7, -30, 30)
	fmt.Fprintf(&b, `<polygon points="%s,%s %s,%s %s,%s %s,%s" fill="#ff8c00"/>`,
		f(gsX-4), f(100+gsDY), f(gsX), f(100+gsDY-5), f(gsX+4), f(100+gsDY), f(gsX), f(100+gsDY+5))

	// LOC dot scale (bottom edge) — CDI lateral deviation.
	locY := 158.0
	if state.HasBudget {
		fmt.Fprintf(&b, `<circle cx="100" cy="%s" r="3.5" fill="none" stroke="rgba(255,255,255,0.5)" stroke-width="1.2"/>`, f(locY))
		for i := 1; i <= 4; i++ {
			dx := dotStep * float64(i)
			fmt.Fprintf(&b, `<circle cx="%s" cy="%s" r="2" fill="rgba(255,255,255,0.35)"/>`, f(100-dx), f(locY))
			fmt.Fprintf(&b, `<circle cx="%s" cy="%s" r="2" fill="rgba(255,255,255,0.35)"/>`, f(100+dx), f(locY))
		}
		locDX := dotStep * clamp(state.DevDays, -maxDefl, maxDefl)
		locColor := cdiDiamondColor(state.DevDays)
		fmt.Fprintf(&b, `<polygon points="%s,%s %s,%s %s,%s %s,%s" fill="%s"/>`,
			f(100+locDX-4), f(locY), f(100+locDX), f(locY-5), f(100+locDX+4), f(locY), f(100+locDX), f(locY+5),
			locColor)
	} else {
		fmt.Fprintf(&b, `<rect x="80" y="%s" width="40" height="18" rx="3" fill="%s"/>`, f(locY-9), gaugeRed)
		fmt.Fprintf(&b, `<text x="100" y="%s" text-anchor="middle" font-size="10" font-weight="700" fill="#fff" letter-spacing="0.14em">NAV</text>`, f(locY+4))
	}

	// FUEL warning flag — top-right on the bezel when runway days < days left.
	if state.FuelWarn {
		b.WriteString(`<rect x="136" y="10" width="38" height="16" rx="3" fill="var(--lamp-warn,#fdf3e0)" stroke="var(--lamp-warn-edge,#d99114)" stroke-width="1.2"/>`)
		b.WriteString(`<text x="155" y="22" text-anchor="middle" font-family="'B612 Mono',monospace" font-size="8" font-weight="700" fill="var(--lamp-warn-edge,#d99114)" letter-spacing="0.1em">FUEL</text>`)
	}

	// Fixed aircraft reticle — V-shape chevron with flat wing tips.
	b.WriteString(`<polyline points="30,100 55,100 100,118" fill="none" stroke="#ff8c00" stroke-width="3.5" stroke-linecap="round" stroke-linejoin="round"/>`)
	b.WriteString(`<polyline points="170,100 145,100 100,118" fill="none" stroke="#ff8c00" stroke-width="3.5" stroke-linecap="round" stroke-linejoin="round"/>`)
	b.WriteString(`<circle cx="100" cy="106" r="4" fill="#ff8c00"/>`)

	// Readout boxes — bottom center, side by side.
	burnFill := "#00ff88"
	if state.Pitch < 0 {
		burnFill = "#ff5c5c"
	}
	b.WriteString(`<rect x="10" y="168" width="84" height="20" rx="3" class="adi-readout"/>`)
	b.WriteString(`<text x="52" y="176" text-anchor="middle" class="adi-readout-label" style="fill:rgba(255,255,255,0.5)">BURN MTD</text>`)
	fmt.Fprintf(&b, `<text x="52" y="186" text-anchor="middle" class="adi-readout-val" style="fill:%s">%s</text>`, burnFill, esc(state.BurnMTD))

	if state.HasBudget {
		devFill := cdiDiamondColor(state.DevDays)
		b.WriteString(`<rect x="106" y="168" width="84" height="20" rx="3" class="adi-readout"/>`)
		b.WriteString(`<text x="148" y="176" text-anchor="middle" class="adi-readout-label" style="fill:rgba(255,255,255,0.5)">CDI DEV</text>`)
		fmt.Fprintf(&b, `<text x="148" y="186" text-anchor="middle" class="adi-readout-val" style="fill:%s">%s</text>`,
			devFill, esc(cdiReadout(state)))
	}

	fmt.Fprintf(&b, `<circle cx="100" cy="100" r="%d" fill="none" class="adi-bezel-ring" stroke-width="1" opacity="0.5"/>`, clipR)
	b.WriteString(`</g></svg>`)
	return b.String()
}

func cdiDiamondColor(devDays float64) string {
	switch {
	case devDays > 3:
		return gaugeRed
	case devDays > 1:
		return gaugeAmber
	case devDays > 0:
		return "#ff8c00"
	default:
		return "#4ade80"
	}
}

// cdiReadout formats the CDI deviation for display. Sign is flipped from
// the internal convention: negative = losing runway days (over budget),
// positive = gaining (under budget). This reads more intuitively: "-0.7d"
// means you're falling behind by 0.7 budget-days.
func cdiReadout(s ADIState) string {
	display := -s.DevDays // flip: over-budget (positive DevDays) shows negative
	sign := "+"
	if display < 0 {
		sign = "-"
		display = -display
	}
	return fmt.Sprintf("%s%.1fd", sign, display)
}

func formatUSD(v float64) string {
	if v >= 1000 {
		return "$" + comma(fmt.Sprintf("%.0f", v))
	}
	return fmt.Sprintf("$%.2f", v)
}

func buildTicks(target, ema14, ema28 float64) []ADITick {
	var ticks []ADITick
	type emaEntry struct {
		label string
		delta float64
	}
	entries := []emaEntry{{"14d", ema14 - target}, {"28d", ema28 - target}}
	spacing := 20.0
	skyIdx := 1
	gndIdx := 1
	for _, e := range entries {
		if e.delta > 0 {
			ticks = append(ticks, ADITick{DY: spacing * float64(gndIdx), Label: "+" + formatUSD(e.delta), Sky: false})
			gndIdx++
		} else if e.delta < 0 {
			ticks = append(ticks, ADITick{DY: -spacing * float64(skyIdx), Label: "-" + formatUSD(math.Abs(e.delta)), Sky: true})
			skyIdx++
		}
	}
	for _, e := range entries {
		if e.delta > 0 {
			ticks = append(ticks, ADITick{DY: spacing * float64(gndIdx), Label: "+" + formatUSD(e.delta*2), Sky: false})
			gndIdx++
		} else if e.delta < 0 {
			ticks = append(ticks, ADITick{DY: -spacing * float64(skyIdx), Label: "-" + formatUSD(math.Abs(e.delta*2)), Sky: true})
			skyIdx++
		}
	}
	return ticks
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
