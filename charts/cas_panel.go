package charts

import (
	"fmt"
	"math"
	"strings"
)

// CASPanelState holds everything the CAS panel needs to render.
type CASPanelState struct {
	Annunciator Annunciator

	// Flight data.
	Target    string  // daily budget allowance, e.g. "$46"
	Commit      string  // committed daily burn from spreads, e.g. "$18"
	SpentToday  string  // swiped today (excluding committed), e.g. "$5"
	TargetVal   float64 // numeric target for AVAIL computation
	CommitVal   float64 // numeric commit for AVAIL computation
	SpentTodVal float64 // numeric spent-today for AVAIL computation
	Reduction   float64 // glideslope corrective: 0 = on glide, >0 = cut $/day

	// Budget markers.
	HasBudget bool
	Consumed  float64 // 0..1 fraction of monthly budget consumed

	// Fuel strip.
	HasFuel bool
	Days14  float64
	Days28  float64
}

// CASPanelSVG renders the full CAS panel: 4 lamps, TARGET/COMMIT/AVAIL/G·S
// flight data, O/M/I markers, and a fuel strip. All in one 200×400 panel.
func CASPanelSVG(s CASPanelState) string {
	const w, h = 200, 400
	a := s.Annunciator

	var b strings.Builder
	svgOpen(&b, w, h, "CAS status panel", `
.cas-panel{fill:#0f1318;stroke:var(--chart-axis,#c6ccd4);stroke-width:1.5;rx:10}
.cas-title{font-size:9px;letter-spacing:2.5px}
.cas-divider{stroke:#262b33;stroke-width:1}
.cas-dme-lbl{fill:#ffb000;font-size:8px;font-weight:700;letter-spacing:0.14em}
.cas-dme-val{fill:#00ff88;font-family:'B612 Mono',var(--chart-font,system-ui,sans-serif);font-size:13px;font-weight:700;letter-spacing:0.04em}
.cas-fuel-label{fill:#8891a0;font-family:'B612 Mono',var(--chart-font,system-ui,sans-serif);font-size:8px;letter-spacing:0.3px}
.cas-fuel-val{font-family:'B612 Mono',var(--chart-font,system-ui,sans-serif);font-size:11px;font-weight:700}
.cas-strip{fill:#14181e;stroke:#262b33;stroke-width:0.5}
.cas-zone-r{fill:rgba(226,75,74,0.15)}
.cas-zone-a{fill:rgba(217,145,20,0.15)}
.cas-zone-g{fill:rgba(99,153,34,0.15)}
`)

	b.WriteString(`<g>`)
	b.WriteString(`<rect x="8" y="8" width="184" height="384" class="cas-panel"/>`)
	b.WriteString(`<text x="100" y="26" text-anchor="middle" class="cas-title" style="fill:#5a6270">CAS · STATUS</text>`)

	// ── Annunciator lamps ──────────────────────────────────────────
	lamps := [4]Lamp{a.Lamp14, a.Lamp28, a.LampSync, a.LampFuel}
	positions := [4][2]float64{{16, 34}, {104, 34}, {16, 94}, {104, 94}}
	for i, lamp := range lamps {
		x, y := positions[i][0], positions[i][1]
		casLamp(&b, x, y, lamp)
	}

	// ── Flight data: TARGET / COMMIT / AVAIL / G·S ─────────────────
	b.WriteString(`<line x1="24" y1="160" x2="176" y2="160" class="cas-divider"/>`)

	// TARGET
	b.WriteString(`<text x="24" y="178" class="cas-dme-lbl">TARGET</text>`)
	fmt.Fprintf(&b, `<text x="176" y="179" text-anchor="end" class="cas-dme-val">%s</text>`, esc(s.Target))

	// COMMIT
	b.WriteString(`<text x="24" y="198" class="cas-dme-lbl">COMMIT</text>`)
	fmt.Fprintf(&b, `<text x="176" y="199" text-anchor="end" class="cas-dme-val">%s</text>`, esc(s.Commit))

	// SPENT
	b.WriteString(`<text x="24" y="218" class="cas-dme-lbl">SPENT</text>`)
	fmt.Fprintf(&b, `<text x="176" y="219" text-anchor="end" class="cas-dme-val">%s</text>`, esc(s.SpentToday))

	// AVAIL = TARGET - COMMIT - SPENT
	avail := s.TargetVal - s.CommitVal - s.SpentTodVal
	availStr := fmtWhole(math.Max(avail, 0))
	availColor := "#00ff88"
	if avail <= 0 {
		availColor = gaugeRed
		availStr = "$0"
	}
	b.WriteString(`<text x="24" y="238" class="cas-dme-lbl">AVAIL</text>`)
	fmt.Fprintf(&b, `<text x="176" y="239" text-anchor="end" class="cas-dme-val" style="fill:%s">%s</text>`, availColor, esc(availStr))

	// G/S — glideslope status
	b.WriteString(`<text x="24" y="258" class="cas-dme-lbl">G/S</text>`)
	if s.Reduction <= 0 {
		b.WriteString(`<text x="176" y="259" text-anchor="end" class="cas-dme-val">OK</text>`)
	} else {
		gsColor := gaugeAmber
		if s.Reduction > 10 {
			gsColor = gaugeRed
		}
		fmt.Fprintf(&b, `<text x="176" y="259" text-anchor="end" class="cas-dme-val" style="fill:%s">−%s</text>`,
			gsColor, esc(fmtCents(s.Reduction)))
	}

	// ── Markers (O/M/I) ────────────────────────────────────────────
	b.WriteString(`<line x1="24" y1="270" x2="176" y2="270" class="cas-divider"/>`)

	for i, m := range totMarkers {
		cx := 42.0 + 58*float64(i)
		cy := 292.0
		lit := s.HasBudget && s.Consumed >= m.pct
		if lit {
			fmt.Fprintf(&b, `<circle cx="%s" cy="%s" r="10" fill="%s"/>`, f(cx), f(cy), m.color)
			fmt.Fprintf(&b, `<text x="%s" y="%s" text-anchor="middle" font-size="10" font-weight="700" style="fill:#fff">%s</text>`, f(cx), f(cy+4), m.letter)
		} else {
			fmt.Fprintf(&b, `<circle cx="%s" cy="%s" r="10" fill="#1a1e25" stroke="#262b33" stroke-width="1.5"/>`, f(cx), f(cy))
			fmt.Fprintf(&b, `<text x="%s" y="%s" text-anchor="middle" font-size="10" font-weight="700" style="fill:#333a44">%s</text>`, f(cx), f(cy+4), m.letter)
		}
		fmt.Fprintf(&b, `<text x="%s" y="%s" text-anchor="middle" font-size="6" style="fill:#5a6270">%.0f%%</text>`, f(cx), f(cy+16), m.pct*100)
	}
	if s.HasBudget {
		fmt.Fprintf(&b, `<text x="176" y="282" text-anchor="end" font-size="7" style="fill:#5a6270;font-family:'B612 Mono',monospace;letter-spacing:1px">%.0f%%</text>`, s.Consumed*100)
	}

	// ── Fuel strip ─────────────────────────────────────────────────
	const (
		stripL    = 16.0
		stripR    = 184.0
		stripH    = 14.0
		fuelFull  = 120.0
		fuelRed   = 30.0
		fuelAmber = 60.0
	)
	stripW := stripR - stripL

	b.WriteString(`<line x1="24" y1="318" x2="176" y2="318" class="cas-divider"/>`)

	if s.HasFuel {
		daysLabel := fuelReadout(s.Days14)
		fuelColor := casFuelColor(s.Days14)
		fmt.Fprintf(&b, `<text x="%s" y="334" class="cas-fuel-label" style="fill:#8891a0">FUEL</text>`, f(stripL))
		fmt.Fprintf(&b, `<text x="%s" y="334" text-anchor="end" class="cas-fuel-val" style="fill:%s">%s<tspan font-size="8" font-weight="600"> DAYS</tspan></text>`,
			f(stripR), fuelColor, esc(daysLabel))

		sy := 340.0
		fmt.Fprintf(&b, `<rect x="%s" y="%s" width="%s" height="%s" rx="2" class="cas-strip"/>`,
			f(stripL), f(sy), f(stripW), f(stripH))
		redW := stripW * (fuelRed / fuelFull)
		amberW := stripW * ((fuelAmber - fuelRed) / fuelFull)
		greenW := stripW * ((fuelFull - fuelAmber) / fuelFull)
		fmt.Fprintf(&b, `<rect x="%s" y="%s" width="%s" height="%s" rx="2" class="cas-zone-r"/>`,
			f(stripL), f(sy), f(redW), f(stripH))
		fmt.Fprintf(&b, `<rect x="%s" y="%s" width="%s" height="%s" class="cas-zone-a"/>`,
			f(stripL+redW), f(sy), f(amberW), f(stripH))
		fmt.Fprintf(&b, `<rect x="%s" y="%s" width="%s" height="%s" class="cas-zone-g"/>`,
			f(stripL+redW+amberW), f(sy), f(greenW), f(stripH))

		fmt.Fprintf(&b, `<text x="%s" y="%s" font-size="8" font-weight="700" style="fill:#5a6270">E</text>`,
			f(stripL), f(sy+stripH+10))
		fmt.Fprintf(&b, `<text x="%s" y="%s" text-anchor="end" font-size="8" font-weight="700" style="fill:#5a6270">F</text>`,
			f(stripR), f(sy+stripH+10))

		days14Clamped := clamp(s.Days14, 0, fuelFull)
		if math.IsInf(s.Days14, 1) {
			days14Clamped = fuelFull
		}
		needleX := stripL + stripW*(days14Clamped/fuelFull)
		fmt.Fprintf(&b, `<rect x="%s" y="%s" width="3" height="%s" rx="1.5" fill="%s"/>`,
			f(needleX-1.5), f(sy+1.5), f(stripH-3), fuelColor)
	} else {
		fmt.Fprintf(&b, `<text x="%s" y="334" class="cas-fuel-label" style="fill:#8891a0">FUEL</text>`, f(stripL))
		fmt.Fprintf(&b, `<text x="%s" y="334" text-anchor="end" font-size="11" font-weight="700" style="fill:%s;font-family:'B612 Mono',monospace;letter-spacing:0.14em">INOP</text>`,
			f(stripR), gaugeRed)
	}

	b.WriteString(`</g></svg>`)
	return b.String()
}

// casFuelColor picks the readout color based on runway days remaining.
func casFuelColor(days float64) string {
	switch {
	case days <= 30:
		return gaugeRed
	case days <= 60:
		return gaugeAmber
	default:
		return gaugeGreen
	}
}
