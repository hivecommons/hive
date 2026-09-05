package hub

import (
	"strings"
	"testing"
)

// The quadrant column lives inside the dashboardHTML raw string, so there is no
// Go function to call. These tests guard the wiring the way the neighbouring
// dashboard tests do: every handler the markup references must exist, and the
// invariants that are silent when broken must stay asserted somewhere CI runs.
//
// "Silent when broken" is the whole point. A miswired kite still draws a
// plausible shape — nothing throws, the operator is just misinformed.

// TestQuadrantAxisOrderMatchesGo pins the browser's axis order to Go's.
//
// The two orders are declared independently, and the small kite in the column
// is readable ONLY because the axes never move. If they drift apart, the column
// and the hover would draw the same hive as two different shapes, and every
// shape an operator had learned would silently mean something else.
func TestQuadrantAxisOrderMatchesGo(t *testing.T) {
	html := dashScript(t)
	want := "var QUADRANT_AXES = ['trust', 'productivity', 'satisfaction', 'efficiency'];"
	if !strings.Contains(html, want) {
		t.Errorf("dashboardHTML is missing %q — the JS axis order may have drifted from QuadrantAxisOrder", want)
	}
	// And the Go side must still be in that order, or the pin above is pinning
	// the wrong thing.
	for i, axis := range []string{AxisTrust, AxisProductivity, AxisSatisfaction, AxisEfficiency} {
		if QuadrantAxisOrder[i] != axis {
			t.Fatalf("QuadrantAxisOrder[%d] = %s, want %s — JS and Go axis orders have diverged",
				i, QuadrantAxisOrder[i], axis)
		}
	}
}

// TestQuadrantColumnWiring asserts the column actually renders and that every
// piece it depends on is present.
func TestQuadrantColumnWiring(t *testing.T) {
	html := dashScript(t)
	for _, snippet := range []string{
		"function quadrantCell(h)",
		"function quadrantSVG(q, fleet, size, labelled)",
		"function quadrantPanelHTML(q)",
		"function quadrantSortValue(h, key)",
		// The cell must be emitted into the row, or the column header would
		// stand over nothing.
		"'<td>' + quadrantCell(h) + '</td>'",
		// The hover panel needs its class styled, or it renders unpositioned
		// over the table.
		".quadrant-cell .quadrant-hover {",
		// Both hover and keyboard focus must open it.
		".quadrant-cell:hover .quadrant-hover, .quadrant-cell:focus-within .quadrant-hover { display: block; }",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("dashboardHTML is missing %q — the quadrant column is not wired", snippet)
		}
	}
}

// TestQuadrantColumnCountConsistent guards the section-separator colspan.
//
// A stale count leaves the separator short and the table visibly ragged, which
// is exactly the kind of break nothing reports.
func TestQuadrantColumnCountConsistent(t *testing.T) {
	html := dashScript(t)
	for _, snippet := range []string{
		"var TOTAL_COLUMNS = 13;",
		"var TOTAL_COLUMNS_HEADER = 13;",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("dashboardHTML is missing %q — adding the Quadrant column requires bumping both counts", snippet)
		}
	}
}

// TestQuadrantSortsReachable asserts every axis sort has a control.
//
// Sorting by a single weak axis is the reason the column exists — it turns the
// table into a worklist — so a missing sub-sort silently removes the feature
// while leaving the column looking complete.
func TestQuadrantSortsReachable(t *testing.T) {
	html := dashScript(t)
	for _, key := range []string{"quadrant", "quadrantTrust", "quadrantEfficiency", "quadrantProductivity"} {
		if !strings.Contains(html, "subSort('"+key+"'") {
			t.Errorf("dashboardHTML has no sub-sort control for %q — that sort is unreachable", key)
		}
	}
}

// TestQuadrantCacheVersionBumped stops a stale cache painting the new column
// empty. The row shape changed, so a v3 cache must be rejected.
func TestQuadrantCacheVersionBumped(t *testing.T) {
	html := dashScript(t)
	if !strings.Contains(html, "var HIVES_CACHE_VERSION = 6;") {
		t.Error("HIVES_CACHE_VERSION was not bumped — a cached pre-restart-telemetry paint would render the Quadrant column empty")
	}
	// The fleet average must be cached WITH the rows, or a cached paint draws
	// kites with no reference polygon and every row reads as an absolute.
	if !strings.Contains(html, "fleetQuadrant: _fleetQuadrant") {
		t.Error("the fleet average is not cached alongside the rows")
	}
	if !strings.Contains(html, "_fleetQuadrant = c.fleetQuadrant || null;") {
		t.Error("the cached fleet average is never restored on a cached paint")
	}
}

// TestQuadrantPayloadCarriesFleetAverage asserts the browser reads the server's
// average rather than deriving one.
//
// Deriving it client-side would average only the rows this caller may SEE,
// silently excluding the ones they may not and moving the reference — so two
// users would measure the same hive against different baselines.
func TestQuadrantPayloadCarriesFleetAverage(t *testing.T) {
	html := dashScript(t)
	if !strings.Contains(html, "_fleetQuadrant = data.fleet_quadrant || null;") {
		t.Error("the dashboard does not read fleet_quadrant from the payload")
	}
}

// TestQuadrantUnscoredRendersDashNotZero guards the labelled hover.
//
// Printing 0 for an unmeasured axis is the exact lie the scored/unscored split
// exists to prevent, and it is invisible — a 0 looks like a real score.
func TestQuadrantUnscoredRendersDashNotZero(t *testing.T) {
	html := dashScript(t)
	if !strings.Contains(html, "var val = '—';") {
		t.Error("the labelled kite does not default an unscored axis to a dash")
	}
	if !strings.Contains(html, "if (a.scored) {") {
		t.Error("the labelled kite prints a score without checking whether the axis was scored")
	}
}

// TestFleetQuadrantHeaderWiring asserts the aggregate renders above the table.
//
// The header is what turns the instrument from per-hive feedback into a
// platform signal — thirty hives collapsed on the same axis is one problem, not
// thirty nudges — so losing it silently reduces the feature to a row decoration.
func TestFleetQuadrantHeaderWiring(t *testing.T) {
	html := dashScript(t)
	for _, snippet := range []string{
		"function fleetQuadrantHeaderHTML()",
		// It must actually be mounted above the table, not merely defined.
		"fleetQuadrantHeaderHTML() +",
		// No fleet ghost behind the aggregate: it IS the fleet, and passing one
		// would draw two identical overlaid polygons.
		"quadrantSVG(q, null, 78, false)",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("dashboardHTML is missing %q — the fleet quadrant header is not wired", snippet)
		}
	}
	// An unscored fleet must render nothing rather than a collapsed chart,
	// which would read as a fleet failing on every axis.
	if !strings.Contains(html, "if (!q || !q.scored_axes) return '';") {
		t.Error("the fleet header does not suppress itself when nothing scored")
	}
}

// TestQuadrantUnscoredCollapsesToCentre is the visual half of the same rule: an
// unscored axis must plot at the origin so the polygon visibly caves in, rather
// than drawing a small symmetric shape that reads as uniformly mediocre.
func TestQuadrantUnscoredCollapsesToCentre(t *testing.T) {
	html := dashScript(t)
	if !strings.Contains(html, "if (!scored) return [cx, cy];") {
		t.Error("quadrantPoint does not collapse an unscored axis to the centre")
	}
}
