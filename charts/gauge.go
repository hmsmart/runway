package charts

import (
	"fmt"
	"math"
	"strings"
)

// Shared geometry for the round instruments. Angles are SVG-screen degrees:
// 180 is left of center, 270 straight up, 360 right — so a half-circle dial
// sweeping 180..360 runs left → top → right, and increasing angle draws
// clockwise (SVG arc sweep-flag 1).

// polar converts a center, radius, and screen angle to a point.
func polar(cx, cy, r, deg float64) (float64, float64) {
	rad := deg * math.Pi / 180
	return cx + r*math.Cos(rad), cy + r*math.Sin(rad)
}

// bandArc writes a colored scale-band arc from angle a0 to a1 (a0 < a1,
// covering less than a full turn) as a stroked path.
func bandArc(b *strings.Builder, cx, cy, r, a0, a1, width float64, color string) {
	x0, y0 := polar(cx, cy, r, a0)
	x1, y1 := polar(cx, cy, r, a1)
	large := 0
	if a1-a0 > 180 {
		large = 1
	}
	fmt.Fprintf(b, `<path d="M%s %sA%s %s 0 %d 1 %s %s" fill="none" stroke="%s" stroke-width="%s"/>`,
		f(x0), f(y0), f(r), f(r), large, f(x1), f(y1), color, f(width))
}

// needleLine writes a straight needle from the hub toward angle deg.
func needleLine(b *strings.Builder, cx, cy, length, deg float64, attrs string) {
	x, y := polar(cx, cy, length, deg)
	fmt.Fprintf(b, `<line x1="%s" y1="%s" x2="%s" y2="%s" %s/>`, f(cx), f(cy), f(x), f(y), attrs)
}

// radialTick writes a tick mark spanning rIn..rOut at angle deg.
func radialTick(b *strings.Builder, cx, cy, rIn, rOut, deg float64, attrs string) {
	x1, y1 := polar(cx, cy, rIn, deg)
	x2, y2 := polar(cx, cy, rOut, deg)
	fmt.Fprintf(b, `<line x1="%s" y1="%s" x2="%s" y2="%s" %s/>`, f(x1), f(y1), f(x2), f(y2), attrs)
}

// bezel writes the standard round-instrument chrome: outer bezel ring and
// inner face circle, matching the VSI's look.
func bezel(b *strings.Builder, cx, cy float64) {
	fmt.Fprintf(b, `<circle cx="%s" cy="%s" r="100" fill="var(--chart-bar,#cdd3da)"/>`, f(cx), f(cy))
	fmt.Fprintf(b, `<circle cx="%s" cy="%s" r="94" fill="var(--chart-heat-0,#e9ecef)"/>`, f(cx), f(cy))
}

// hub writes the needle pivot cap.
func hub(b *strings.Builder, cx, cy float64) {
	fmt.Fprintf(b, `<circle cx="%s" cy="%s" r="7" fill="var(--chart-heat-0,#e9ecef)" stroke="var(--chart-ink,#33383f)" stroke-width="1.5"/>`, f(cx), f(cy))
	fmt.Fprintf(b, `<circle cx="%s" cy="%s" r="3" fill="var(--chart-ink,#33383f)"/>`, f(cx), f(cy))
}

// Status colors shared by the banded gauges and the annunciator lamps:
// green nominal, amber caution, red warning.
const (
	gaugeGreen = "#639922"
	gaugeAmber = "#d99114"
	gaugeRed   = "#e24b4a"
)
