package charts

import (
	"fmt"
	"strings"
)

// DMERow is one readout line on the DME box: a short annunciator-style label
// and a preformatted value.
type DMERow struct {
	Label string
	Value string
}

// DME renders the digital readout box: a black instrument face with a stack
// of LED-style figures. Unlike the round gauges the face stays black in both
// themes — that's the instrument's identity.
func DME(rows []DMERow) string {
	var b strings.Builder
	svgOpen(&b, 200, 200, "flight data readouts", `
.dme .lbl{fill:#ffb000;font-size:8px;font-weight:700;letter-spacing:0.14em}
.dme .val{fill:#00ff88;font-family:'B612 Mono',monospace;font-size:15px;font-weight:700;letter-spacing:0.04em}
`)
	b.WriteString(`<g class="dme">`)
	b.WriteString(`<rect x="6" y="6" width="188" height="188" rx="12" fill="var(--chart-bar,#cdd3da)"/>`)
	b.WriteString(`<rect x="14" y="14" width="172" height="172" rx="8" fill="#101214" stroke="#000" stroke-width="1"/>`)
	b.WriteString(`<text x="100" y="36" text-anchor="middle" font-size="9" font-weight="700" fill="#6a7280" letter-spacing="0.18em">FLT DATA</text>`)

	y := 66.0
	for _, r := range rows {
		fmt.Fprintf(&b, `<text x="28" y="%s" class="lbl">%s</text>`, f(y), esc(r.Label))
		fmt.Fprintf(&b, `<text x="172" y="%s" text-anchor="end" class="val">%s</text>`, f(y+1), esc(r.Value))
		fmt.Fprintf(&b, `<line x1="28" y1="%s" x2="172" y2="%s" stroke="#22262b" stroke-width="1"/>`, f(y+12), f(y+12))
		y += 34
	}

	b.WriteString(`</g></svg>`)
	return b.String()
}
