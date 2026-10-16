package hub

import (
	"strings"
	"testing"
)

// quadrantExplainerFixtureJS is one axis as /api/hub/dashboard serializes it
// after the evidence-fed rewrite: two measured criteria, one not measured yet.
const quadrantExplainerFixtureJS = `{
  axis: 'trust', score: 62, scored: true, measured: 2, unmeasured: 1,
  summary: 'Trust 62: reverts are rare (2.0%) but only 6.0% of merges have evidence yet · 1 of 3 criteria not measured yet',
  criteria: [
    {name: 'revert_rate', label: 'Revert rate', measured: true, value: 0.02, display: '2.0%', score: 80, weight: 2,
     how: 'Revert rate: 10.0% or worse scores 0, 0.0% or better scores 100, linear between.', good: 'Under 2% of merges reverted within 14 days.'},
    {name: 'evidence_coverage', label: 'Evidence coverage', measured: true, value: 0.06, display: '6.0%', score: 6, weight: 1,
     how: 'Evidence coverage: 0.0% scores 0, 100.0% scores 100, linear between.', good: 'Every merge carries an evidence record.'},
    {name: 'false_approve_rate', label: 'False-approve rate <b>', measured: false, weight: 2, reason: 'Not measured yet: fewer than 10 reviewer verdicts in 90 days',
     how: 'False-approve rate: 5.0% or worse scores 0, 0.0% scores 100.', good: 'Under 1%.'}
  ]
}`

func quadrantExplainerHarness(t *testing.T) string {
	t.Helper()
	return strings.Join([]string{
		"function esc(s) { return String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;'); }",
		"function escAttr(s) { return esc(s).replace(/\"/g,'&quot;'); }",
		"var QUADRANT_AXES = ['trust', 'productivity', 'satisfaction', 'efficiency'];",
		countryJSDecl(t, "QUADRANT_AXIS_LABELS"),
		countryJSDecl(t, "QUADRANT_AXIS_FULL"),
		countryJSDecl(t, "_quadrantExplainerSeq"),
		jsFunc(t, "quadrantAxis"),
		jsFunc(t, "quadrantExplainerId"),
		jsFunc(t, "quadrantCriteriaListHTML"),
		jsFunc(t, "quadrantExplainerHTML"),
		jsFunc(t, "quadrantInfoButtonHTML"),
		jsFunc(t, "quadrantAxisTileHTML"),
		jsFunc(t, "quadrantSummaryHTML"),
		jsFunc(t, "quadrantCloseExplainers"),
		jsFunc(t, "quadrantToggleExplainer"),
	}, "\n")
}

// TestQuadrantExplainerRendersCriteria: the ⓘ popover lists every criterion
// with its value and sub-score, marks unmeasured ones with the server's
// reason (never a 0), and carries the four plain-language sections.
func TestQuadrantExplainerRendersCriteria(t *testing.T) {
	src := quadrantExplainerHarness(t) + "\n" +
		"var a = " + quadrantExplainerFixtureJS + ";\n" +
		"var tile = quadrantAxisTileHTML(a, 'fleet');\n" +
		"var unscored = quadrantAxisTileHTML({axis: 'satisfaction', scored: false, reason: 'Not measured yet: no reviewer verdicts', criteria: []}, 'fleet');\n" +
		"result = {tile: tile, unscored: unscored};"
	var got struct{ Tile, Unscored string }
	runEvidenceJS(t, src, &got)

	for _, want := range []string{
		`class="quadrant-axis"`,
		`class="quadrant-axis-label">Trust<button type="button" class="quadrant-info" aria-expanded="false" aria-controls="quadrant-explainer-fleet-trust-`,
		`class="quadrant-axis-score">62</span>`,
		`class="quadrant-explainer" role="region" aria-label="About the Trust axis" hidden`,
		`<h4>What this covers</h4>`,
		`<h4>How it's scored</h4>`, `<h4>What good looks like</h4>`, `<h4>What ours shows</h4>`,
		`<li class="is-measured"><span class="quadrant-criterion-label">Revert rate</span><span class="quadrant-criterion-value">2.0%</span><span class="quadrant-criterion-score">80</span><span class="quadrant-criterion-weight">×2</span></li>`,
		`<li class="is-unmeasured"><span class="quadrant-criterion-label">False-approve rate &lt;b&gt;</span><span class="quadrant-criterion-reason">Not measured yet: fewer than 10 reviewer verdicts in 90 days</span>`,
		"left out of the average rather than counted as zero",
		"Revert rate: 10.0% or worse scores 0",
		"Under 2% of merges reverted within 14 days.",
		"reverts are rare (2.0%) but only 6.0% of merges have evidence yet",
		`onclick="event.stopPropagation();quadrantToggleExplainer(this, 'quadrant-explainer-fleet-trust-`,
	} {
		if !strings.Contains(got.Tile, want) {
			t.Errorf("explainer is missing %q:\n%s", want, got.Tile)
		}
	}
	// An unmeasured criterion's how/good text must not be sold as what the
	// score is based on while measured criteria exist.
	if strings.Contains(got.Tile, "False-approve rate: 5.0%") {
		t.Errorf("unmeasured criterion leaked into 'How it's scored':\n%s", got.Tile)
	}
	if strings.Contains(got.Tile, `quadrant-criterion-score">0</span>`) {
		t.Errorf("unmeasured criterion rendered as a 0 score:\n%s", got.Tile)
	}
	for _, want := range []string{
		`class="quadrant-axis-score is-unscored">—</span>`,
		`<h4>What ours shows</h4><p>Not measured yet: no reviewer verdicts</p>`,
		"No criteria reported for this axis yet.",
	} {
		if !strings.Contains(got.Unscored, want) {
			t.Errorf("unscored tile is missing %q:\n%s", want, got.Unscored)
		}
	}
}

// TestQuadrantExplainerToggle: one popover open at a time, aria-expanded
// tracks it, and quadrantCloseExplainers reports whether it closed anything
// so the Escape handler can consume the key only when it did.
func TestQuadrantExplainerToggle(t *testing.T) {
	src := quadrantExplainerHarness(t) + "\n" + `
var els = {};
function El(id) { this.id = id; this.hidden = true; this.attrs = {}; }
El.prototype.setAttribute = function(k, v) { this.attrs[k] = v; };
var btn1 = new El('b1'), btn2 = new El('b2');
var ex1 = new El('x1'), ex2 = new El('x2');
els.x1 = ex1; els.x2 = ex2;
var document = {
  getElementById: function(id) { return els[id] || null; },
  querySelectorAll: function(sel) {
    if (sel.indexOf('quadrant-explainer') >= 0) return [ex1, ex2].filter(function(e) { return !e.hidden; });
    return [btn1, btn2].filter(function(b) { return b.attrs['aria-expanded'] === 'true'; });
  }
};
var steps = [];
quadrantToggleExplainer(btn1, 'x1');
steps.push([ex1.hidden, ex2.hidden, btn1.attrs['aria-expanded']]);
quadrantToggleExplainer(btn2, 'x2');
steps.push([ex1.hidden, ex2.hidden, btn1.attrs['aria-expanded'], btn2.attrs['aria-expanded']]);
quadrantToggleExplainer(btn2, 'x2');
steps.push([ex1.hidden, ex2.hidden, btn2.attrs['aria-expanded']]);
quadrantToggleExplainer(btn1, 'x1');
var closedSomething = quadrantCloseExplainers();
var closedNothing = quadrantCloseExplainers();
quadrantToggleExplainer(btn1, 'missing');
result = {steps: steps, closedSomething: closedSomething, closedNothing: closedNothing, finalHidden: ex1.hidden};`
	var got struct {
		Steps           [][]interface{}
		ClosedSomething bool
		ClosedNothing   bool
		FinalHidden     bool
	}
	runEvidenceJS(t, src, &got)
	if len(got.Steps) != 3 {
		t.Fatalf("steps: %+v", got.Steps)
	}
	if got.Steps[0][0] != false || got.Steps[0][1] != true || got.Steps[0][2] != "true" {
		t.Errorf("opening x1: %+v", got.Steps[0])
	}
	if got.Steps[1][0] != true || got.Steps[1][1] != false || got.Steps[1][2] != "false" || got.Steps[1][3] != "true" {
		t.Errorf("opening x2 must close x1: %+v", got.Steps[1])
	}
	if got.Steps[2][1] != true || got.Steps[2][2] != "false" {
		t.Errorf("toggling x2 again must close it: %+v", got.Steps[2])
	}
	if !got.ClosedSomething || got.ClosedNothing || !got.FinalHidden {
		t.Errorf("close semantics: %+v", got)
	}
}

// TestQuadrantSummaryPicksWeakestAxis: the one-line reading under the glyph
// is the weakest scored axis's summary, and nothing when no axis has one.
func TestQuadrantSummaryPicksWeakestAxis(t *testing.T) {
	src := quadrantExplainerHarness(t) + "\n" + `
var q = {scored_axes: 2, axes: [
  {axis: 'trust', scored: true, score: 70, summary: 'Trust 70: fine'},
  {axis: 'efficiency', scored: true, score: 30, summary: 'Efficiency 30: tokens per merge are high'},
  {axis: 'satisfaction', scored: false, reason: 'Not measured yet'}
]};
result = {some: quadrantSummaryHTML(q), none: quadrantSummaryHTML({scored_axes: 0, axes: []}), nil: quadrantSummaryHTML(null)};`
	var got struct{ Some, None, Nil string }
	runEvidenceJS(t, src, &got)
	if got.Some != `<div class="quadrant-summary">Efficiency 30: tokens per merge are high</div>` {
		t.Errorf("summary: %q", got.Some)
	}
	if got.None != "" || got.Nil != "" {
		t.Errorf("empty summary expected, got %q / %q", got.None, got.Nil)
	}
}

// TestQuadrantExplainersWiredIntoBothSurfaces: fleet card and per-hive hover
// share the tile renderer, the fleet card states its aggregate and n, and
// Escape closes a quadrant explainer before dismissing a modal.
func TestQuadrantExplainersWiredIntoBothSurfaces(t *testing.T) {
	html := dashScript(t)
	for _, want := range []string{
		"return quadrantAxisTileHTML(quadrantAxis(q, name), 'fleet'); }).join('');",
		"return quadrantAxisTileHTML(quadrantAxis(q, name), 'hive'); }).join('');",
		"var agg = (q.aggregate || 'median') + (q.n ? ' of ' + q.n : '');",
		`<span class="fleet-quadrant-n">· ' + esc(agg) + '</span>`,
		"quadrantSummaryHTML(q) +",
		"if (typeof quadrantCloseExplainers === 'function' && quadrantCloseExplainers()) { e.preventDefault(); e.stopPropagation(); return; }",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboard script is missing %q", want)
		}
	}
	if strings.Count(html, "quadrantSummaryHTML(q) +") != 2 {
		t.Error("the plain one-line summary must sit under both the fleet glyph and the per-hive hover")
	}
	for _, cls := range []string{".quadrant-info {", ".quadrant-explainer {", ".quadrant-explainer[hidden]", ".quadrant-criteria li.is-unmeasured", ".quadrant-summary {", ".quadrant-axis-score.is-unscored"} {
		if !strings.Contains(dashboardHTML, cls) {
			t.Errorf("dashboard CSS is missing %q", cls)
		}
	}
}
