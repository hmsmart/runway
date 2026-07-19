package charts

import (
	"fmt"
	"math"
	"strings"
)

// GaugeScale describes the three breakpoints used by the VSI dial.
// Inner is the dense linear region, Mid is the bend, and Outer is the
// clamp point where the needle stops.
type GaugeScale struct {
	Inner float64
	Mid   float64
	Outer float64
}

var defaultGaugeScale = GaugeScale{Inner: 5, Mid: 10, Outer: 50}

// VSI renders a vertical-speed-style gauge for a current value and a sample
// series used to derive the scale breakpoints. Percentiles are taken over the
// absolute values of the series, then rounded to a stable display grid.
func VSI(values []float64, current float64) string {
	scale := gaugeScaleFromValues(values)
	return renderVSI(current, scale)
}

func gaugeScaleFromValues(values []float64) GaugeScale {
	if len(values) == 0 {
		return defaultGaugeScale
	}
	p85, err := percentileInc(values, 85)
	if err != nil {
		return defaultGaugeScale
	}
	p95, err := percentileInc(values, 95)
	if err != nil {
		return defaultGaugeScale
	}
	p995, err := percentileInc(values, 99.5)
	if err != nil {
		return defaultGaugeScale
	}
	return normalizeGaugeScale(roundToStep(p85, 0.5), roundToStep(p95, 0.5), roundToStep(p995, 1))
}

func normalizeGaugeScale(inner, mid, outer float64) GaugeScale {
	inner = math.Max(0.5, math.Abs(inner))
	mid = math.Max(inner+0.5, math.Abs(mid))
	outer = math.Max(mid+0.5, math.Abs(outer))
	return GaugeScale{Inner: inner, Mid: mid, Outer: outer}
}

func roundToStep(v, step float64) float64 {
	if step <= 0 {
		return v
	}
	return math.Round(v/step) * step
}

func fmtTick(v float64) string {
	rounded := math.Round(v)
	if math.Abs(v-rounded) < 0.001 {
		return fmt.Sprintf("%.0f", rounded)
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", v), ".0")
}

func gaugeValueToAngle(v float64, scale GaugeScale) float64 {
	clamped := math.Max(-scale.Outer, math.Min(scale.Outer, v))
	signed := clamped
	absV := math.Abs(signed)
	sign := 1.0
	if signed < 0 {
		sign = -1
	}

	if absV <= scale.Inner {
		return 180 - (signed/scale.Inner)*90
	}

	if absV <= scale.Mid {
		span := (absV - scale.Inner) / (scale.Mid - scale.Inner)
		if sign > 0 {
			return 90 - span*30
		}
		return 270 + span*30
	}

	span := (absV - scale.Mid) / (scale.Outer - scale.Mid)
	if sign > 0 {
		return 60 - span*40
	}
	return 300 + span*40
}

func gaugeTicks(scale GaugeScale) ([]float64, []float64) {
	var majors []float64
	majors = append(majors, 0)
	innerFloor := int(math.Floor(scale.Inner))
	maxIntMajor := innerFloor
	// If the inner breakpoint is fractional, skip the last integer label to
	// avoid a crowded pair like 6 and 6.5 at the bend.
	if math.Abs(scale.Inner-float64(innerFloor)) > 0.001 {
		maxIntMajor = innerFloor - 1
	}
	for i := 1; i <= maxIntMajor; i++ {
		majors = append(majors, float64(i), float64(-i))
	}
	if math.Abs(scale.Inner-float64(innerFloor)) > 0.001 {
		majors = append(majors, scale.Inner, -scale.Inner)
	} else if innerFloor > 0 {
		majors = append(majors, float64(innerFloor), float64(-innerFloor))
	}

	var minors []float64
	for v := 0.5; v < scale.Inner; v += 1 {
		minors = append(minors, v, -v)
	}
	return majors, minors
}

func signedDailyValue(v float64) string {
	if v == 0 {
		return "$0.00"
	}
	if v > 0 {
		return fmt.Sprintf("▼$%.2f", v)
	}
	return fmt.Sprintf("▲$%.2f", math.Abs(v))
}

func renderVSI(current float64, scale GaugeScale) string {
	const (
		w, h   = 200, 200
		cx     = 100.0
		cy     = 100.0
		bezel  = 100.0
		face   = 94.0
		tickR  = 94.0
		needle = 78.0
	)

	var b strings.Builder
	svgOpen(&b, w, h, "vertical speed gauge", `
.vsi text{font-family:var(--chart-font,system-ui,-apple-system,'Segoe UI',sans-serif)}
.vsi .label{fill:var(--chart-muted,#6a7280)}
	.vsi .readout-window{fill:var(--chart-heat-0,#e9ecef);stroke:var(--chart-axis,#c6ccd4);stroke-width:1.5}
	.vsi .readout{fill:var(--chart-ink,#33383f);font-family:'B612 Mono',var(--chart-font,system-ui,-apple-system,'Segoe UI',sans-serif);font-size:13px;font-weight:700;letter-spacing:0.02em}
	.vsi .readout-unit{font-size:9px;font-weight:600;letter-spacing:0}
.vsi .needlep{stroke:#e24b4a;stroke-width:2.5;stroke-linecap:round}
`)
	b.WriteString(`<g class="vsi">`)
	fmt.Fprintf(&b, `<circle cx="%s" cy="%s" r="%s" fill="var(--chart-bar,#cdd3da)"/>`, f(cx), f(cy), f(bezel))
	fmt.Fprintf(&b, `<circle cx="%s" cy="%s" r="%s" fill="var(--chart-heat-0,#e9ecef)"/>`, f(cx), f(cy), f(face))

	majors, minors := gaugeTicks(scale)
	for _, v := range majors {
		a := gaugeValueToAngle(v, scale) * math.Pi / 180
		len := 10.0
		wgt := 1.5
		if v == 0 || math.Abs(math.Abs(v)-scale.Inner) < 0.001 {
			len = 14
			wgt = 2
		}
		x1 := cx + tickR*math.Cos(a)
		y1 := cy + tickR*math.Sin(a)
		x2 := cx + (tickR-len)*math.Cos(a)
		y2 := cy + (tickR-len)*math.Sin(a)
		fmt.Fprintf(&b, `<line x1="%s" y1="%s" x2="%s" y2="%s" stroke="var(--chart-ink,#33383f)" stroke-width="%s" stroke-linecap="round"/>`, f(x1), f(y1), f(x2), f(y2), f(wgt))
		tx := cx + (tickR-len-11)*math.Cos(a)
		ty := cy + (tickR-len-11)*math.Sin(a)
		label := fmtTick(math.Abs(v))
		fmt.Fprintf(&b, `<text x="%s" y="%s" font-size="10" font-weight="700" text-anchor="middle" fill="var(--chart-muted,#6a7280)">%s</text>`, f(tx), f(ty+3.5), esc(label))
	}
	for _, v := range minors {
		a := gaugeValueToAngle(v, scale) * math.Pi / 180
		x1 := cx + tickR*math.Cos(a)
		y1 := cy + tickR*math.Sin(a)
		x2 := cx + (tickR-6)*math.Cos(a)
		y2 := cy + (tickR-6)*math.Sin(a)
		fmt.Fprintf(&b, `<line x1="%s" y1="%s" x2="%s" y2="%s" stroke="var(--chart-ink,#33383f)" stroke-width="1" stroke-linecap="round" opacity="0.8"/>`, f(x1), f(y1), f(x2), f(y2))
	}

	for _, v := range []float64{scale.Mid, scale.Outer, -scale.Mid, -scale.Outer} {
		a := gaugeValueToAngle(v, scale) * math.Pi / 180
		tx := cx + (tickR-20)*math.Cos(a)
		ty := cy + (tickR-20)*math.Sin(a)
		fmt.Fprintf(&b, `<text x="%s" y="%s" font-size="9" font-weight="700" text-anchor="middle" fill="var(--chart-muted,#6a7280)">%s</text>`, f(tx), f(ty+3.5), esc(fmtTick(math.Abs(v))))
	}

	// Dial text: up/down hints around the needle.
	fmt.Fprintf(&b, `<text x="100" y="50" text-anchor="middle" font-size="9" font-weight="700" fill="var(--chart-muted,#6a7280)" letter-spacing="0.08em">UP</text>`)
	fmt.Fprintf(&b, `<text x="100" y="160" text-anchor="middle" font-size="9" font-weight="700" fill="var(--chart-muted,#6a7280)" letter-spacing="0.08em">DN</text>`)
	fmt.Fprintf(&b, `<rect x="52" y="118" width="96" height="24" rx="4" class="readout-window"/>`)
	fmt.Fprintf(&b, `<text x="100" y="134" text-anchor="middle" class="readout"><tspan>%s</tspan><tspan class="readout-unit">/day</tspan></text>`, esc(signedDailyValue(current)))

	needleAngle := gaugeValueToAngle(current, scale) * math.Pi / 180
	xn := cx + needle*math.Cos(needleAngle)
	yn := cy + needle*math.Sin(needleAngle)
	fmt.Fprintf(&b, `<line x1="%s" y1="%s" x2="%s" y2="%s" class="needlep"/>`, f(cx), f(cy), f(xn), f(yn))
	fmt.Fprintf(&b, `<circle cx="%s" cy="%s" r="7" fill="var(--chart-heat-0,#e9ecef)" stroke="var(--chart-ink,#33383f)" stroke-width="1.5"/>`, f(cx), f(cy))
	fmt.Fprintf(&b, `<circle cx="%s" cy="%s" r="3" fill="var(--chart-ink,#33383f)"/>`, f(cx), f(cy))

	b.WriteString(`</g>`)
	b.WriteString("</svg>")
	return b.String()
}
