package hub

import (
	"strings"
	"testing"
)

// Render tests. A wrong kite still draws a plausible shape, so every failure
// mode here is silent: nothing throws, the operator is just misinformed.

// scored builds a quadrant with explicit per-axis scores for render tests.
func scored(t, p, s, e int, satisfactionScored bool) Quadrant {
	q := Quadrant{Axes: []AxisScore{
		{Axis: AxisTrust, Score: t, Scored: true},
		{Axis: AxisProductivity, Score: p, Scored: true},
		{Axis: AxisSatisfaction, Score: s, Scored: satisfactionScored},
		{Axis: AxisEfficiency, Score: e, Scored: true},
	}}
	q.ScoredAxes = 3
	if satisfactionScored {
		q.ScoredAxes = 4
	}
	return q
}

// TestUnscoredAxisCollapsesToCentre is the visual half of the absent-evidence
// rule. If an unscored axis ever plots away from the centre, "not measured"
// becomes indistinguishable from a real low score.
func TestUnscoredAxisCollapsesToCentre(t *testing.T) {
	cx, cy := 50.0, 50.0
	x, y := quadrantPoint(2, 0, false, cx, cy, 40)
	if x != cx || y != cy {
		t.Errorf("unscored axis plotted at (%.2f,%.2f), want centre (%.2f,%.2f)", x, y, cx, cy)
	}
	// Positive control: a SCORED zero must also sit at the centre by geometry,
	// but a scored 50 must not — proving the collapse is not just "everything
	// lands at the centre".
	if x50, y50 := quadrantPoint(2, 50, true, cx, cy, 40); x50 == cx && y50 == cy {
		t.Error("a scored 50 collapsed to the centre — geometry is degenerate")
	}
}

// TestNorthIsUp pins orientation. SVG's y axis grows downward, so a missing
// negation silently flips the whole chart top-to-bottom and every learned shape
// means its mirror image.
func TestNorthIsUp(t *testing.T) {
	cx, cy := 50.0, 50.0
	_, yTrust := quadrantPoint(0, 100, true, cx, cy, 40) // Trust = north
	if yTrust >= cy {
		t.Errorf("Trust plotted at y=%.2f, not above centre %.2f — the chart is upside down", yTrust, cy)
	}
	_, ySat := quadrantPoint(2, 100, true, cx, cy, 40) // Satisfaction = south
	if ySat <= cy {
		t.Errorf("Satisfaction plotted at y=%.2f, not below centre %.2f", ySat, cy)
	}
	xProd, _ := quadrantPoint(1, 100, true, cx, cy, 40) // Productivity = east
	if xProd <= cx {
		t.Errorf("Productivity plotted at x=%.2f, not right of centre %.2f", xProd, cx)
	}
	xEff, _ := quadrantPoint(3, 100, true, cx, cy, 40) // Efficiency = west
	if xEff >= cx {
		t.Errorf("Efficiency plotted at x=%.2f, not left of centre %.2f", xEff, cx)
	}
}

// TestUnscoredShowsDashNotZero guards the labelled hover. Printing "0" for an
// unmeasured axis is precisely the lie the whole sufficiency design exists to
// prevent.
func TestUnscoredShowsDashNotZero(t *testing.T) {
	svg := quadrantSVG(scored(70, 60, 0, 50, false), Quadrant{}, 180, true)
	if !strings.Contains(svg, "—") {
		t.Error("labelled kite with an unscored axis shows no dash — it must not print a number")
	}
	// The unscored axis must not contribute a vertex dot.
	if got := strings.Count(svg, "<circle"); got != 3 {
		t.Errorf("expected 3 vertex dots for 3 scored axes, got %d", got)
	}
}

// TestSmallKiteHasNoLabels keeps the table column readable. Labels at 22px
// would be illegible and would blow out the column width.
func TestSmallKiteHasNoLabels(t *testing.T) {
	svg := quadrantSVG(scored(70, 60, 40, 50, true), Quadrant{}, 22, false)
	if strings.Contains(svg, "<text") {
		t.Error("small kite emitted text labels — the column must be shape-only")
	}
	if !strings.Contains(svg, "<polygon") {
		t.Error("small kite emitted no polygon at all")
	}
}

// TestFleetGhostOmittedWhenEmpty stops an empty reference polygon collapsing to
// a dot at the centre of every row, which would read as a data point.
func TestFleetGhostOmittedWhenEmpty(t *testing.T) {
	withGhost := quadrantSVG(scored(70, 60, 40, 50, true), scored(50, 50, 50, 50, true), 22, false)
	noGhost := quadrantSVG(scored(70, 60, 40, 50, true), Quadrant{}, 22, false)
	if strings.Count(withGhost, "<polygon") <= strings.Count(noGhost, "<polygon") {
		t.Error("fleet ghost polygon was not drawn when a fleet average was supplied")
	}
	if strings.Contains(noGhost, "stroke-dasharray") {
		t.Error("empty fleet average still drew a dashed reference polygon")
	}
}

// TestAriaLabelDescribesEveryAxis keeps the kite non-visually accessible; a
// shape-only SVG is otherwise entirely invisible to a screen reader.
func TestAriaLabelDescribesEveryAxis(t *testing.T) {
	svg := quadrantSVG(scored(70, 60, 0, 50, false), Quadrant{}, 22, false)
	for _, want := range []string{"Trust", "Prod", "Satis", "Effic"} {
		if !strings.Contains(svg, want) {
			t.Errorf("aria-label omits %s", want)
		}
	}
	if !strings.Contains(svg, "not measured") {
		t.Error("aria-label does not distinguish an unmeasured axis")
	}
	empty := quadrantSVG(Quadrant{}, Quadrant{}, 22, false)
	if !strings.Contains(empty, "not enough data") {
		t.Error("a fully unscored quadrant must say so in its aria-label")
	}
}

// TestRenderEscapesText guards the raw-string renderer against unescaped
// interpolation, matching the package convention of html.EscapeString.
func TestRenderEscapesText(t *testing.T) {
	q := scored(70, 60, 40, 50, true)
	q.Axes[0].Axis = `"><script>alert(1)</script>`
	svg := quadrantSVG(q, Quadrant{}, 180, true)
	if strings.Contains(svg, "<script>") {
		t.Error("axis name was interpolated unescaped into the SVG")
	}
}

// TestNoPanicOnPartialQuadrant covers a struct missing axes entirely, which a
// future caller could easily produce.
func TestNoPanicOnPartialQuadrant(t *testing.T) {
	partial := Quadrant{Axes: []AxisScore{{Axis: AxisTrust, Score: 80, Scored: true}}, ScoredAxes: 1}
	svg := quadrantSVG(partial, Quadrant{}, 180, true)
	if !strings.Contains(svg, "<svg") {
		t.Error("partial quadrant did not render")
	}
	if got := strings.Count(svg, "<circle"); got != 1 {
		t.Errorf("expected exactly 1 vertex dot, got %d", got)
	}
}
