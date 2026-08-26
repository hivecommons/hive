package hub

import (
	"fmt"
	"html"
	"math"
	"strings"
)

// SVG rendering for the quadrant kite.
//
// ONE renderer, three mounts. The table column, the column hover, and the
// status-panel hover all call quadrantSVG with a different size and detail
// level. They must never drift into separate implementations: the small kite is
// only readable because it is literally the same shape as the large one, and an
// operator who learns a shape in the hover must find that shape in the column.
//
// Rendered server-side into the dashboard markup rather than drawn in JS,
// matching how the rest of this dashboard is built.

// Kite geometry. The axes sit at the four compass points in canonical order —
// Trust north, Productivity east, Satisfaction south, Efficiency west — which
// is what makes a lopsided hive recognisable at a glance.
//
// Angles are measured clockwise from north, in degrees, aligned index-for-index
// with QuadrantAxisOrder.
var quadrantAxisAngles = []float64{0, 90, 180, 270}

// quadrantPoint maps an axis index and a 0-100 score to an SVG coordinate.
//
// An unscored axis collapses to the exact centre. That is deliberate and is the
// visual expression of the absent-evidence rule: the polygon visibly caves in
// on that side rather than rendering a small-but-symmetric shape that would
// read as "uniformly mediocre" instead of "not measured".
func quadrantPoint(axisIdx int, score int, scored bool, cx, cy, radius float64) (float64, float64) {
	if !scored {
		return cx, cy
	}
	frac := float64(score) / 100
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	rad := quadrantAxisAngles[axisIdx%len(quadrantAxisAngles)] * math.Pi / 180
	// -cos for y because SVG's y axis grows downward; north must point up.
	return cx + radius*frac*math.Sin(rad), cy - radius*frac*math.Cos(rad)
}

// quadrantPolygon builds the "x,y x,y ..." points attribute for one quadrant.
func quadrantPolygon(q Quadrant, cx, cy, radius float64) string {
	pts := make([]string, 0, len(QuadrantAxisOrder))
	for i, axis := range QuadrantAxisOrder {
		as := axisByName(q, axis)
		x, y := quadrantPoint(i, as.Score, as.Scored, cx, cy, radius)
		pts = append(pts, fmt.Sprintf("%.2f,%.2f", x, y))
	}
	return strings.Join(pts, " ")
}

// axisByName finds an axis on a quadrant, returning a zero-valued unscored axis
// when absent so a partially-populated struct can never panic a render.
func axisByName(q Quadrant, axis string) AxisScore {
	for _, a := range q.Axes {
		if a.Axis == axis {
			return a
		}
	}
	return AxisScore{Axis: axis}
}

// quadrantSVG renders the kite.
//
// size is the square edge in px. labelled adds axis initials, scores and the
// delta-vs-fleet annotations; the table column passes false (shape only), the
// hovers pass true.
//
// fleet is the reference polygon drawn faintly behind the hive's own, so each
// kite reads as a DEVIATION FROM NORMAL rather than an absolute the viewer has
// to calibrate mentally. Pass a zero Quadrant to omit it.
//
// Colours come from the dashboard's palette tokens only (--accent, --muted,
// --line, --text) rather than literals, so the kite follows the palette if it
// is ever retuned. The hub dashboard is currently single-theme (dark); there is
// no prefers-color-scheme or data-theme switch to track, so this is about
// staying on the maintained palette, not about theme adaptation.
func quadrantSVG(q Quadrant, fleet Quadrant, size float64, labelled bool) string {
	// Leave room for labels; unlabelled kites use nearly the whole box.
	pad := 2.0
	if labelled {
		pad = size * 0.22
	}
	cx, cy := size/2, size/2
	radius := size/2 - pad

	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" width="%.0f" height="%.0f" `+
		`role="img" aria-label="%s" style="display:block;overflow:visible">`,
		size, size, size, size, quadrantAriaLabel(q))

	// Grid: concentric reference rings at 33/66/100%, plus the four spokes.
	// Kept very faint so they orient the eye without competing with the data.
	for _, ring := range []float64{0.33, 0.66, 1.0} {
		pts := make([]string, 0, 4)
		for i := range QuadrantAxisOrder {
			x, y := quadrantPoint(i, 100, true, cx, cy, radius*ring)
			pts = append(pts, fmt.Sprintf("%.2f,%.2f", x, y))
		}
		fmt.Fprintf(&b, `<polygon points="%s" fill="none" stroke="var(--line)" `+
			`stroke-width="0.5" opacity="0.35"/>`, strings.Join(pts, " "))
	}
	if labelled {
		for i := range QuadrantAxisOrder {
			x, y := quadrantPoint(i, 100, true, cx, cy, radius)
			fmt.Fprintf(&b, `<line x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" `+
				`stroke="var(--line)" stroke-width="0.5" opacity="0.35"/>`, cx, cy, x, y)
		}
	}

	// Fleet reference polygon, behind the hive's own.
	if fleet.ScoredAxes > 0 {
		fmt.Fprintf(&b, `<polygon points="%s" fill="var(--muted)" fill-opacity="0.10" `+
			`stroke="var(--muted)" stroke-width="0.75" stroke-dasharray="2,2" opacity="0.6"/>`,
			quadrantPolygon(fleet, cx, cy, radius))
	}

	// The hive's own kite.
	fmt.Fprintf(&b, `<polygon points="%s" fill="var(--accent)" fill-opacity="0.22" `+
		`stroke="var(--accent)" stroke-width="1.5" stroke-linejoin="round"/>`,
		quadrantPolygon(q, cx, cy, radius))

	// A dot on each scored vertex reads better than the polygon alone at small
	// sizes, where a shallow angle can be hard to see.
	for i, axis := range QuadrantAxisOrder {
		as := axisByName(q, axis)
		if !as.Scored {
			continue
		}
		x, y := quadrantPoint(i, as.Score, true, cx, cy, radius)
		r := 1.5
		if labelled {
			r = 2.5
		}
		fmt.Fprintf(&b, `<circle cx="%.2f" cy="%.2f" r="%.1f" fill="var(--accent)"/>`, x, y, r)
	}

	if labelled {
		for i, axis := range QuadrantAxisOrder {
			as := axisByName(q, axis)
			lx, ly := quadrantPoint(i, 118, true, cx, cy, radius)
			anchor := "middle"
			switch quadrantAxisAngles[i] {
			case 90:
				anchor = "start"
			case 270:
				anchor = "end"
			}
			fmt.Fprintf(&b, `<text x="%.2f" y="%.2f" text-anchor="%s" `+
				`font-size="8" fill="var(--muted)" style="text-transform:uppercase;letter-spacing:0.5px">%s</text>`,
				lx, ly, anchor, html.EscapeString(axisShortLabel(axis)))
			// Score sits just under the label. An unscored axis shows a dash
			// rather than a 0, so "not measured" never reads as a bad score.
			val := "—"
			if as.Scored {
				val = fmt.Sprintf("%d", as.Score)
				if as.Delta != 0 {
					val = fmt.Sprintf("%d %s%d", as.Score, deltaSign(as.Delta), abs(as.Delta))
				}
			}
			fmt.Fprintf(&b, `<text x="%.2f" y="%.2f" text-anchor="%s" `+
				`font-size="7.5" fill="var(--text)" opacity="0.85">%s</text>`,
				lx, ly+9, anchor, html.EscapeString(val))
		}
	}

	b.WriteString(`</svg>`)
	return b.String()
}

// axisShortLabel is the compact axis name used on the labelled kite.
func axisShortLabel(axis string) string {
	switch axis {
	case AxisTrust:
		return "Trust"
	case AxisEfficiency:
		return "Effic"
	case AxisSatisfaction:
		return "Satis"
	case AxisProductivity:
		return "Prod"
	}
	return axis
}

// deltaSign renders the direction of a delta explicitly, so "34 -21" cannot be
// misread as a range.
func deltaSign(d int) string {
	if d < 0 {
		return "−" // U+2212 minus, not a hyphen: it aligns with digits
	}
	return "+"
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// quadrantAriaLabel gives the SVG a text equivalent, since a shape-only kite is
// otherwise invisible to a screen reader.
func quadrantAriaLabel(q Quadrant) string {
	if q.ScoredAxes == 0 {
		return "Quadrant: not enough data"
	}
	parts := make([]string, 0, len(QuadrantAxisOrder))
	for _, axis := range QuadrantAxisOrder {
		as := axisByName(q, axis)
		if as.Scored {
			parts = append(parts, fmt.Sprintf("%s %d", axisShortLabel(axis), as.Score))
		} else {
			parts = append(parts, fmt.Sprintf("%s not measured", axisShortLabel(axis)))
		}
	}
	return html.EscapeString("Quadrant: " + strings.Join(parts, ", "))
}
