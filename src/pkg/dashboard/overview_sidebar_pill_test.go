package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestOverviewSidebarPillMirrorsTileTotals runs renderOverviewKPIs under node
// and asserts the left-nav Overview "x/y" pill is the very same two numbers
// the "Total open issues" / "Total open PRs" tiles print — from the same
// /api/status snapshot, on the same render pass — for the server-totals
// path, the per-repo fallback path, the stale/last-good path, and with the
// KPI strip hidden. Before this the pill counted stage-tracker rows (tracked
// items only), so it read 24/11 while the tiles read 31/18.
func TestOverviewSidebarPillMirrorsTileTotals(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: Overview sidebar pill rule was not executed")
	}
	html := indexHTML(t)
	script := overviewKPIRenderPrelude(t, html) + `
const pillEl = () => document.getElementById('overview-sidebar-badge');
const tiles = out => Object.fromEntries([...out.matchAll(/data-overview-kpi-key="([^"]+)" data-overview-kpi-value="([^"]+)"/g)].map(m => [m[1], Number(m[2])]));
const state = { showKPIs: true };
const issueSlices = [
  { key: 'ready', count: 2, items: [] }, { key: 'in-progress', count: 1, items: [] }, { key: 'agent-filed', count: 0, items: [] },
  { key: 'waiting', count: 3, items: [] }, { key: 'done', count: 0, items: [] },
];
const prSlices = [
  { key: 'waiting', count: 1, items: [] }, { key: 'eligible', count: 0, items: [] }, { key: 'blocked', count: 2, items: [] },
  { key: 'in-review', count: 0, items: [] }, { key: 'open', count: 0, items: [] }, { key: 'draft', count: 0, items: [] },
];

// 1. Hosted path: /api/status carries overviewTotals (the owner's 31/18 case,
// where the stage tracker only knew about 24 issues / 11 PRs).
// Rows carry the server's partition term (overview_term); the KPI render
// derives its equation from them when the status carries none.
const repos = [{ name: 'hivecommons/hive', issues: 20, prs: 7,
  actionableIssues: [{ number: 4, overview_term: 'actionable' }, { number: 5, overview_term: 'actionable' }, { number: 6, overview_term: 'actionable' }],
  heldIssues: [{ number: 1, overview_term: 'held' }, { number: 2, overview_term: 'held' }], heldPrs: [{ number: 3, overview_term: 'held' }] }];
_overviewLastRepos = repos;
window._lastStatus = {
  repos,
  overviewTotals: {
    issues: { forge: 31, tracked: 6, held: 2, outside: 23, breakdown: { needs_human: 1, filtered: 22 } },
    prs: { forge: 18, tracked: 3, held: 1, outside: 14, breakdown: { draft: 4, filtered: 10 } },
  },
  actionableNow: { total: 3, issues: 3, prs: 0 },
};
let out = renderOverviewKPIs(repos, issueSlices, prSlices, state);
let kpi = tiles(out);
assert.equal(kpi['open-issues'], 31);
assert.equal(kpi['open-prs'], 18);
assert.equal(pillEl().hidden, false);
assert.equal(pillEl().textContent, '31/18', 'pill must print the tile totals, not the tracked working set');
assert.equal(pillEl().textContent, kpi['open-issues'] + '/' + kpi['open-prs']);
assert.ok(!('data-zero' in pillEl().attrs));
assert.match(pillEl().title, /^31 open issues \/ 18 open PRs — the Hive activity monitor pill counts rows of this same set \(every issue, plus each PR not shown under a listed issue\), so it can read differently · 31 total open issues \+ 18 total open PRs = 3 actionable now \+ /, 'pill title names each half, says how the activity-monitor pill differs, then carries the tile equation');
assert.doesNotMatch(pillEl().title, /stale/);

// 2. Stale / last-good: GitHub refused the enumeration, the tiles show the
// last successful read with a stale badge — the pill shows the same numbers
// and says so in its title.
window._lastStatus.staleSince = '2026-10-21T15:04:05Z';
out = renderOverviewKPIs(repos, issueSlices, prSlices, state);
kpi = tiles(out);
assert.match(out, /overview-kpi-stale/);
assert.equal(pillEl().textContent, kpi['open-issues'] + '/' + kpi['open-prs']);
assert.equal(pillEl().textContent, '31/18');
assert.match(pillEl().title, /\(stale since .+ · serving last-good\)$/);
delete window._lastStatus.staleSince;

// 3. Per-repo fallback (no overviewTotals): the tiles sum repo forge counts,
// and the pill follows them through the same change.
delete window._lastStatus.overviewTotals;
repos[0].issues = 12;
repos[0].prs = 5;
out = renderOverviewKPIs(repos, issueSlices, prSlices, state);
kpi = tiles(out);
assert.equal(kpi['open-issues'], 12);
assert.equal(kpi['open-prs'], 5);
assert.equal(pillEl().textContent, '12/5');

// 4. KPI strip hidden by the user: no tiles are rendered, but the pill keeps
// following the same numbers the tiles would print.
repos[0].issues = 9;
repos[0].prs = 0;
out = renderOverviewKPIs(repos, issueSlices, prSlices, { showKPIs: false });
assert.equal(out, '');
assert.equal(pillEl().textContent, '9/0');

// 5. Before /api/status has answered the pill stays untouched (hidden), so a
// chart-only render never flashes 0/0.
document._els = {};
window._lastStatus = null;
renderOverviewKPIs([], [], [], state);
assert.equal(pillEl().hidden, true);
assert.equal(pillEl().textContent, '0/0');

// 6. Zero is a real answer once the feed has answered.
window._lastStatus = { repos: [], overviewTotals: { issues: { forge: 0 }, prs: { forge: 0 } } };
renderOverviewKPIs([], [], [], state);
assert.equal(pillEl().hidden, false);
assert.equal(pillEl().textContent, '0/0');
assert.equal(pillEl().attrs['data-zero'], '');
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node overview sidebar pill render failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
