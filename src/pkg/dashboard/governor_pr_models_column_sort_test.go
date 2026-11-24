package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// The PRs by model header columns sort on click: most to least first, the
// same column again flips the direction, rows whose run columns render a dash
// (no runs recorded) sort last both ways, ties break by PR count and then
// model, and the rank / PR count pills clear a column sort. The comparator is
// EXECUTED here, not pattern-matched.

func govPRModelColumnsConst(t *testing.T, html string) string {
	t.Helper()
	const decl = "const GOV_PR_MODEL_COLUMNS = ["
	start := strings.Index(html, decl)
	if start < 0 {
		t.Fatal("index.html does not define GOV_PR_MODEL_COLUMNS")
	}
	end := strings.Index(html[start:], "\n    ];")
	if end < 0 {
		t.Fatal("unterminated GOV_PR_MODEL_COLUMNS in index.html")
	}
	return html[start : start+end+len("\n    ];")]
}

func TestGovernorPRModelsColumnSortWiring(t *testing.T) {
	html := indexHTML(t)
	tile := jsFunc(t, html, "renderGovernorPRModelsTile")
	for _, want := range []string{"governorPRModelSortHeadHtml(", "gov-pr-models-colsort", "setGovernorPRModelsColumnSort"} {
		if !strings.Contains(tile, want) {
			t.Errorf("renderGovernorPRModelsTile does not use %q", want)
		}
	}
	for _, want := range []string{
		"window.setGovernorPRModelsColumnSort = setGovernorPRModelsColumnSort;",
		`data-action="setGovernorPRModelsColumnSort"`,
		"gov-pr-model-sort-arrow",
		".gov-pr-model-sort:focus-visible",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}

func TestGovernorPRModelsColumnSortOrders(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH: the PRs by model column sort was NOT executed")
	}
	html := indexHTML(t)
	var script strings.Builder
	script.WriteString(`
const assert = require('node:assert/strict');
let govPRModelsSort = 'effectiveness';
let govPRModelsColumnSort = { key: '', dir: 'desc' };
const renders = [];
const window = { _lastStatus: { governor: {}, cadenceMatrix: [] } };
function renderGovernor() { renders.push(govPRModelsColumnSort.key + ':' + govPRModelsColumnSort.dir); }
const focusLog = [];
let activeClass = '';
const document = {
  get activeElement() { return { classList: { contains: c => c === activeClass } }; },
  querySelector: sel => ({ focus() { focusLog.push(sel); } }),
};
function escapeHtml(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;'); }
`)
	script.WriteString(govPRModelColumnsConst(t, html) + "\n")
	for _, name := range []string{
		"renderGovernorFromLastStatus",
		"setGovernorPRModelsSort",
		"setGovernorPRModelsColumnSort",
		"governorPRModelUnattributed",
		"governorPRModelSortLabel",
		"governorPRModelColumnValue",
		"governorPRModelColumnCompare",
		"governorPRModelSortHeadHtml",
		"governorPRModelRows",
	} {
		script.WriteString(jsFunc(t, html, name) + "\n")
	}
	script.WriteString(`
const row = (model, prs, fail, runs, extra = {}) => ({ model, backend: 'copilot', prs, merged: extra.merged ?? 0,
  rework: { first_pass_merge_rate: extra.rwFirstPass },
  effectiveness: { runs, failure_rate: fail, verified_pr_run_rate: extra.prRuns, nothing_to_ship_rate: extra.noShip, rank: extra.rank, merged_prs: extra.mergedPRs, first_pass_merge_rate: extra.firstPass, unattributed: extra.unattributed } });
const data = { buckets: [
  row('alpha', 3, 0.2, 4, { rank: 2 }),
  row('beta', 5, 0.5, 6, { rank: 1 }),
  row('gamma', 7, 0, 0),                          // no runs recorded: fail renders a dash
  row('delta', 3, 0.2, 2),                        // ties alpha on fail and PR count
  row('epsilon', 9, 0.2, 8),                      // ties on fail, more PRs
  row('unknown', 4, 0.9, 3, { unattributed: true }),
  row('zeta', 2, 0.1, 0),                         // no runs, fail field set: still a dash
  row('eta', 1, 0.05, 5),
] };
const names = () => governorPRModelRows(data).map(b => b.model);

// Rank is the default; unattributed is pinned last under rank.
assert.equal(govPRModelsColumnSort.key, '');
assert.equal(names()[0], 'beta');
assert.equal(names().at(-1), 'unknown');

// First click: most to least. Dash rows (no runs) go last, then the
// unattributed row; fail ties break by PR count (epsilon 9 first), then
// model (alpha before delta).
setGovernorPRModelsColumnSort('fail');
assert.deepEqual(govPRModelsColumnSort, { key: 'fail', dir: 'desc' });
assert.deepEqual(names(), ['beta', 'epsilon', 'alpha', 'delta', 'eta', 'gamma', 'zeta', 'unknown']);

// Second click: least to most. Dash rows and the unattributed row stay last;
// ties still break by PR count then model.
setGovernorPRModelsColumnSort('fail');
assert.deepEqual(govPRModelsColumnSort, { key: 'fail', dir: 'asc' });
assert.deepEqual(names(), ['eta', 'epsilon', 'alpha', 'delta', 'beta', 'gamma', 'zeta', 'unknown']);

// Third click flips back.
setGovernorPRModelsColumnSort('fail');
assert.deepEqual(govPRModelsColumnSort, { key: 'fail', dir: 'desc' });

// Another column starts most to least again.
setGovernorPRModelsColumnSort('prs');
assert.deepEqual(govPRModelsColumnSort, { key: 'prs', dir: 'desc' });
assert.deepEqual(names(), ['epsilon', 'gamma', 'beta', 'alpha', 'delta', 'zeta', 'eta', 'unknown']);

// Rates compare as the whole percent shown: 20.4% and 20.1% both show 20%,
// so they tie and the row with more PRs goes first, in both directions.
const close = { buckets: [row('few', 2, 0.204, 5), row('many', 6, 0.201, 5), row('low', 4, 0.15, 5)] };
govPRModelsColumnSort = { key: 'fail', dir: 'desc' };
assert.deepEqual(governorPRModelRows(close).map(b => b.model), ['many', 'few', 'low']);
govPRModelsColumnSort = { key: 'fail', dir: 'asc' };
assert.deepEqual(governorPRModelRows(close).map(b => b.model), ['low', 'many', 'few']);
assert.equal(governorPRModelColumnValue({ rework: { first_pass_merge_rate: 0.956 } }, 'first_pass'), 96);

// Merged and first pass read the effectiveness value, falling back to the bucket / rework value.
assert.equal(governorPRModelColumnValue({ merged: 4, effectiveness: {} }, 'merged'), 4);
assert.equal(governorPRModelColumnValue({ merged: 4, effectiveness: { merged_prs: 6 } }, 'merged'), 6);
assert.equal(governorPRModelColumnValue({ rework: { first_pass_merge_rate: 0.4 } }, 'first_pass'), 40);
assert.equal(governorPRModelColumnValue({ rework: { first_pass_merge_rate: 0.4 }, effectiveness: { first_pass_merge_rate: 0.7 } }, 'first_pass'), 70);
for (const key of ['pr_runs', 'fail', 'no_ship']) {
  assert.equal(governorPRModelColumnValue({ effectiveness: { runs: 0, failure_rate: 0.3, verified_pr_run_rate: 0.3, nothing_to_ship_rate: 0.3 } }, key), null);
}

// The button's aria-label carries the sort state (no aria-sort: the header is
// a div grid, not a table); the arrow marks it visually, and the help title
// and ? mark stay outside the button.
govPRModelsColumnSort = { key: '', dir: 'desc' };
setGovernorPRModelsColumnSort('no_ship');
let head = governorPRModelSortHeadHtml('no_ship', 'help text', 'gov-pr-model-metric');
assert.match(head, /^<div class="gov-pr-model-metric"><span class="gov-pr-model-col" title="help text"><button type="button" class="gov-pr-model-sort active" data-action="setGovernorPRModelsColumnSort" data-arg0="no_ship" aria-label="No ship, sorted most to least. Activate to reverse.">/);
assert.match(head, /No ship<span class="gov-pr-model-sort-arrow" aria-hidden="true">▾<\/span><\/button><span class="gov-pr-model-help" aria-hidden="true">\?<\/span>/);
setGovernorPRModelsColumnSort('no_ship');
head = governorPRModelSortHeadHtml('no_ship', 'help text', 'gov-pr-model-metric');
assert.match(head, /aria-label="No ship, sorted least to most. Activate to reverse."/);
assert.match(head, /▴/);
const idle = governorPRModelSortHeadHtml('fail', 'help', 'gov-pr-model-metric');
assert.doesNotMatch(idle, /sort-arrow| active/);
assert.match(idle, /aria-label="Sort by Fail"/);
assert.doesNotMatch(head + idle, /aria-sort|[\u2013\u2014\u0040]/);

// Model sorts A to Z first; the unattributed "unknown" row stays last both
// ways instead of landing among the u's.
setGovernorPRModelsColumnSort('model');
assert.deepEqual(govPRModelsColumnSort, { key: 'model', dir: 'asc' });
assert.deepEqual(names(), ['alpha', 'beta', 'delta', 'epsilon', 'eta', 'gamma', 'zeta', 'unknown']);
const us = { buckets: [row('ultra', 1, 0, 0), row('unknown', 1, 0, 0, { unattributed: true }), row('zeta', 1, 0, 0), row('alpha', 1, 0, 0)] };
assert.deepEqual(governorPRModelRows(us).map(b => b.model), ['alpha', 'ultra', 'zeta', 'unknown']);
setGovernorPRModelsColumnSort('model');
assert.deepEqual(govPRModelsColumnSort, { key: 'model', dir: 'desc' });
assert.deepEqual(governorPRModelRows(us).map(b => b.model), ['zeta', 'ultra', 'alpha', 'unknown']);

// Keyboard focus survives the re-render: on the header button, or on the
// active column-sort pill when that was used.
activeClass = 'gov-pr-model-sort';
setGovernorPRModelsColumnSort('merged');
activeClass = 'gov-pr-models-colsort';
setGovernorPRModelsColumnSort('merged');
activeClass = '';
setGovernorPRModelsColumnSort('merged');
assert.deepEqual(focusLog, ['.gov-pr-model-head .gov-pr-model-sort[data-arg0="merged"]', '.gov-pr-models-colsort']);
setGovernorPRModelsColumnSort('model');

// Unknown keys are ignored.
setGovernorPRModelsColumnSort('bogus');
assert.deepEqual(govPRModelsColumnSort, { key: 'model', dir: 'asc' });

// Choosing rank (or PR count) clears the column sort; rank pins unattributed last again.
setGovernorPRModelsColumnSort('fail');
setGovernorPRModelsSort('effectiveness');
assert.deepEqual(govPRModelsColumnSort, { key: '', dir: 'desc' });
assert.equal(govPRModelsSort, 'effectiveness');
assert.equal(names()[0], 'beta');
assert.equal(names().at(-1), 'unknown');
setGovernorPRModelsColumnSort('fail');
setGovernorPRModelsSort('count');
assert.equal(govPRModelsColumnSort.key, '');
assert.equal(names()[0], 'epsilon');
assert.ok(renders.length > 0, 'sort changes re-render the governor card');
`)
	if out, err := exec.Command(node, "-e", script.String()).CombinedOutput(); err != nil {
		t.Fatalf("PRs by model column sort check failed:\n%s", strings.TrimSpace(string(out)))
	}
}
