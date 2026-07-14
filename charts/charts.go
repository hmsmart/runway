// Package charts renders the dashboard's SVG charts. Renderers are pure
// (data in, SVG document out) so they can be exercised without a database.
//
// Theming contract: every color in these SVGs is a CSS custom property
// (--chart-* for chrome and EMA series, --cat-* for spend categories) with a
// hex fallback baked in here. Each SVG @imports the site stylesheet, which is
// the single place the theme defines (and can re-plumb) those properties for
// light and dark. Embed the endpoints with <object>/<iframe> — an <img> is
// forbidden from fetching the stylesheet, so it would fall back to the
// light-mode defaults below.
package charts

import (
	"fmt"
	"html"
	"math"
	"strings"
	"time"
)

// Day is one row of the daily-spend series. EMA pointers are nil only for
// legacy rows that predate a smoothing column; renderers skip those points.
type Day struct {
	Date  string // YYYY-MM-DD
	Spend float64
	E14   *float64
	E28   *float64
	E84   *float64
}

// Category is one primary finance category's smoothed daily rates.
type Category struct {
	Name string // Plaid PFC primary, e.g. FOOD_AND_DRINK; "" means uncategorized
	E14  float64
	E28  float64
	E84  float64
	// Recent is true when the category saw spending inside the shortest EMA
	// window (the last 14 days) — the condition under which all three
	// horizons carry live signal rather than just decay.
	Recent bool
}

// View selects which EMA horizon a single-series chart (the donut) shows.
type View string

const (
	ViewEMA14 View = "ema14"
	ViewEMA28 View = "ema28"
	ViewEMA84 View = "ema84"
)

// ParseView maps a ?view= query value to a View, defaulting to the 14-day
// horizon for anything unrecognized.
func ParseView(s string) View {
	switch strings.ToLower(s) {
	case "ema28":
		return ViewEMA28
	case "ema84":
		return ViewEMA84
	default:
		return ViewEMA14
	}
}

// emaSlot describes one EMA horizon's presentation: its CSS class in the SVG,
// the custom property the theme overrides, and the validated ordinal-blue
// fallbacks (14-day lightest, 84-day darkest — depth encodes horizon).
type emaSlot struct {
	view          View
	label         string
	class         string
	cssVar        string
	fallbackLight string
}

var emaSlots = [3]emaSlot{
	{ViewEMA14, "ema14", "ema14", "--chart-ema14", "#6da7ec"},
	{ViewEMA28, "ema28", "ema28", "--chart-ema28", "#256abf"},
	{ViewEMA84, "ema84", "ema84", "--chart-ema84", "#0d366b"},
}

// categoryFallback maps each Plaid primary category to its default hex. The
// first seven are the validated categorical palette in fixed order; the tail
// reuses those hue families at shifted lightness (rare in a spend chart, and
// the donut folds past the top five into Other anyway). The theme overrides
// any of these via --cat-<slug>.
var categoryFallback = map[string]string{
	"FOOD_AND_DRINK":            "#1baf7a",
	"GENERAL_MERCHANDISE":       "#eda100",
	"TRANSPORTATION":            "#008300",
	"RENT_AND_UTILITIES":        "#4a3aa7",
	"ENTERTAINMENT":             "#e34948",
	"TRAVEL":                    "#e87ba4",
	"PERSONAL_CARE":             "#eb6834",
	"GENERAL_SERVICES":          "#0e7a52",
	"MEDICAL":                   "#a52727",
	"HOME_IMPROVEMENT":          "#b57b00",
	"LOAN_PAYMENTS":             "#7f71d1",
	"BANK_FEES":                 "#005c00",
	"GOVERNMENT_AND_NON_PROFIT": "#c25579",
	"TRANSFER_OUT":              "#bf4a1a",
	"INCOME":                    "#557799",
	"TRANSFER_IN":               "#778899",
}

const otherFallback = "#8f96a3"

// OtherName is the synthetic category the tail folds into.
const OtherName = "OTHER"

// catSlug converts a Plaid primary category to the kebab-case token used in
// CSS class names and custom properties: FOOD_AND_DRINK -> food-and-drink.
func catSlug(name string) string {
	if name == "" || name == OtherName {
		return "other"
	}
	return strings.ToLower(strings.ReplaceAll(name, "_", "-"))
}

// catLabel renders a category for humans in the site's lowercase voice:
// FOOD_AND_DRINK -> "food & drink".
func catLabel(name string) string {
	if name == "" || name == OtherName {
		return "other"
	}
	s := strings.ToLower(strings.ReplaceAll(name, "_", " "))
	return strings.ReplaceAll(s, " and ", " & ")
}

// catColorRule emits the CSS rule tying a category class to its theme
// variable, with the documented fallback.
func catColorRule(name string) string {
	slug := catSlug(name)
	fb, ok := categoryFallback[name]
	if !ok {
		fb = otherFallback
	}
	return fmt.Sprintf(".cat-%s{color:var(--cat-%s,%s)}", slug, slug, fb)
}

// svgOpen starts an SVG document sized by viewBox and writes the shared
// style block. Marks reference their series color via class + currentColor,
// so one rule serves fills and strokes alike. The chart's name goes in an
// aria-label rather than a root <title>: same accessible name, but no native
// tooltip popping over the chart on hover.
func svgOpen(b *strings.Builder, w, h int, title string, extraCSS string) {
	fmt.Fprintf(b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" role="img" aria-label="%s">`, w, h, esc(title))
	b.WriteString("<style>@import url('/assets/css/style.css');")
	b.WriteString("text{font-family:var(--chart-font,system-ui,-apple-system,'Segoe UI',sans-serif);fill:var(--chart-ink,#33383f)}")
	b.WriteString(".muted{fill:var(--chart-muted,#6a7280)}")
	b.WriteString(".grid{stroke:var(--chart-grid,#e3e6ea);stroke-width:1}")
	b.WriteString(".axis{stroke:var(--chart-axis,#c6ccd4);stroke-width:1}")
	b.WriteString(".bar{fill:var(--chart-bar,#cdd3da)}")
	for _, s := range emaSlots {
		fmt.Fprintf(b, ".%s{color:var(%s,%s)}", s.class, s.cssVar, s.fallbackLight)
	}
	// Hover cells: pointing at a .cell reveals its .readout text and rings
	// its .mark — the shared no-JS tooltip mechanism (heatmap days, trend
	// bars).
	b.WriteString(".cell .readout{display:none}")
	b.WriteString(".cell:hover .readout{display:block}")
	b.WriteString(".cell:hover .mark{stroke:var(--chart-ink,#33383f);stroke-width:1.5}")
	b.WriteString(extraCSS)
	b.WriteString("</style>")
}

func esc(s string) string { return html.EscapeString(s) }

// fmtCents renders a dollar amount for tooltips: $84.21.
func fmtCents(v float64) string {
	return "$" + comma(fmt.Sprintf("%.2f", v))
}

// fmtWhole renders a rounded dollar amount for axis ticks and tip labels.
func fmtWhole(v float64) string {
	return "$" + comma(fmt.Sprintf("%.0f", v))
}

// comma inserts thousands separators into the integer part of a plain
// numeric string.
func comma(s string) string {
	intPart, frac, _ := strings.Cut(s, ".")
	n := len(intPart)
	if n <= 3 {
		if frac != "" {
			return intPart + "." + frac
		}
		return intPart
	}
	var out strings.Builder
	lead := n % 3
	if lead > 0 {
		out.WriteString(intPart[:lead])
	}
	for i := lead; i < n; i += 3 {
		if out.Len() > 0 {
			out.WriteString(",")
		}
		out.WriteString(intPart[i : i+3])
	}
	if frac != "" {
		out.WriteString("." + frac)
	}
	return out.String()
}

// niceMax rounds v up to a clean axis maximum. Steps are chosen to quarter
// cleanly, since axes and radar rings divide the max into four.
func niceMax(v float64) float64 {
	if v <= 0 {
		return 1
	}
	mag := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 1.2, 1.6, 2, 2.4, 3, 4, 4.8, 6, 8, 10} {
		if v <= m*mag {
			return m * mag
		}
	}
	return 10 * mag
}

// f trims a coordinate to one decimal so paths stay compact.
func f(v float64) string {
	s := fmt.Sprintf("%.1f", v)
	return strings.TrimSuffix(s, ".0")
}

// dayLabel formats a YYYY-MM-DD date for tooltips: "tue jun 30".
func dayLabel(date string) string {
	t, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return date
	}
	return strings.ToLower(t.Format("Mon Jan 2"))
}
