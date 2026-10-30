package dashboard

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// overviewKPIRenderPrelude is the node harness that lets renderOverviewKPIs
// run outside the browser: constants, state, a localStorage/document stub and
// every helper the KPI strip (and the sidebar Overview pill it writes) calls.
func overviewKPIRenderPrelude(t *testing.T, html string) string {
	t.Helper()
	return `const assert = require('node:assert/strict');
const OVERVIEW_KPI_LOCAL_PREFIX = 'hive.overviewKPI.local.';
const OVERVIEW_KPI_LOCAL_MAX = 2016;
const OVERVIEW_KPI_MIN_SAMPLE_MS = 5 * 60 * 1000;
const OVERVIEW_KPI_WINDOW_KEY = 'hive.overviewKPI.window';
const OVERVIEW_KPI_WINDOWS = { '24h': 24 * 3600e3, '7d': 7 * 24 * 3600e3 };
let _overviewKPIHistory = [];
let _overviewKPIWindow = '24h';
let _overviewKPILocalScope = 'all';
let _overviewLastRepos = [];
const window = { _lastStatus: {} };
const location = { hash: '', search: '' };
const OVERVIEW_ISSUE_BAND_ORDER = ['ready', 'in-progress', 'agent-filed', 'waiting', 'done'];
const PR_BAND_ORDER = ['waiting', 'eligible', 'blocked', 'in-review', 'open', 'draft'];
const OVERVIEW_PROJECT_FILTERS = {
  'open-issues': { label: 'Total open issues', kinds: ['issue'], bucketKind: 'issues' },
  'open-prs': { label: 'Total open PRs', kinds: ['pr'], bucketKind: 'prs' },
  'actionable-now': { label: 'Actionable now', kinds: ['issue', 'pr'], term: 'actionable' },
  'your-turn': { label: 'Your turn', kinds: ['issue', 'pr'], term: 'your_turn' },
  'waiting-elsewhere': { label: 'Waiting on someone else', kinds: ['issue', 'pr'], term: 'waiting_elsewhere' },
  'held': { label: 'Held', kinds: ['issue', 'pr'], term: 'held' },
  'blocked': { label: 'Blocked', kinds: ['pr'], term: 'blocked' },
  'outside': { label: 'Outside', kinds: ['issue', 'pr'], term: 'outside', outside: true }
};
let _overviewProjectFilterKey = null;
let _overviewProjectFilterCount = null;
function repoItemNeedsHuman(item){ return ((item && item.labels) || []).some(l => String(l).toLowerCase() === 'needs-human'); }
const localStorage = { data: {}, getItem(k){ return Object.prototype.hasOwnProperty.call(this.data,k) ? this.data[k] : null; }, setItem(k,v){ this.data[k]=String(v); }, removeItem(k){ delete this.data[k]; } };
function fmtSparkVal(v){ return String(v); }
function renderSparkline(_, __, opts){ return '<svg data-id="' + ((opts && opts.id) || '') + '"></svg>'; }
const OVERVIEW_ISSUE_BREAKDOWN_LABELS = { needs_human: 'needs-human', needs_direction: 'needs-direction', needs_decision: 'needs-decision', needs_spec: 'needs-spec', exempt: 'exempt', filtered: 'filtered', reporter_triage: 'reporter triage', hive_advisory: 'hive advisory', dependency_dashboard: 'dependency dashboard', other: 'other' };
const OVERVIEW_PR_BREAKDOWN_LABELS = { hold: 'held', draft: 'draft', filtered: 'filtered', other: 'other' };
` + jsFunc(t, html, "esc") + `
` + jsFunc(t, html, "overviewBreakdownTotal") + `
` + jsFunc(t, html, "overviewRepoForgeTotals") + `
` + jsFunc(t, html, "overviewKPIForgeTotals") + `
` + jsFunc(t, html, "overviewKPIBreakdownSubline") + `
` + jsFunc(t, html, "overviewKPITerm") + `
` + jsFunc(t, html, "dashboardDocsHref") + `
` + jsFunc(t, html, "dashboardDocsHrefFromPath") + `
` + jsFunc(t, html, "overviewPartitionDocsLink") + `
` + jsFunc(t, html, "overviewPartitionSettingsButton") + `
` + jsFunc(t, html, "overviewPartitionSummaryLine") + `
` + jsFunc(t, html, "overviewPartitionGloss") + `
` + jsFunc(t, html, "overviewPartitionPreferredDocs") + `
` + jsFunc(t, html, "overviewPartitionPreferredSettings") + `
` + jsFunc(t, html, "overviewPartitionRowHTML") + `
` + jsFunc(t, html, "overviewPartitionTooltip") + `
` + jsFunc(t, html, "overviewPartitionInfo") + `
` + jsFunc(t, html, "overviewActionableTermHTML") + `
` + jsFunc(t, html, "renderActionableEquationSubline") + `
` + jsFunc(t, html, "overviewEquationTermCount") + `
` + jsFunc(t, html, "overviewOutsideBreakdownText") + `
` + jsFunc(t, html, "overviewStaleSince") + `
` + jsFunc(t, html, "fmtOverviewStaleSince") + `
` + jsFunc(t, html, "overviewRestartRefresh") + `
` + jsFunc(t, html, "overviewStaleBadgeText") + `
` + jsFunc(t, html, "relativeTimeShort") + `
` + jsFunc(t, html, "renderOverviewTotalPartitionSubline") + `
` + jsFunc(t, html, "renderOverviewSplitSubline") + `
` + jsFunc(t, html, "overviewEquationTermDefs") + `
` + jsFunc(t, html, "overviewKPICombinedEquation") + `
` + jsFunc(t, html, "overviewKPILocalEquations") + `
` + jsFunc(t, html, "overviewKPIKindEquation") + `
` + jsFunc(t, html, "overviewKPIRepoScope") + `
` + jsFunc(t, html, "overviewKPILocalKey") + `
` + jsFunc(t, html, "overviewKPILoadLocal") + `
` + jsFunc(t, html, "overviewKPISaveLocal") + `
` + jsFunc(t, html, "overviewKPIRecordLocal") + `
` + jsFunc(t, html, "overviewKPIHistoryEntries") + `
` + jsFunc(t, html, "overviewKPICurrentSample") + `
` + jsFunc(t, html, "overviewKPIWindowControls") + `
` + jsFunc(t, html, "overviewKPISparkTitle") + `
` + jsFunc(t, html, "overviewKPISpark") + `
` + jsFunc(t, html, "overviewCoverageInfo") + `
` + jsFunc(t, html, "overviewCoverageState") + `
` + jsFunc(t, html, "overviewCoverageColor") + `
` + jsFunc(t, html, "overviewCoverageInfoHTML") + `
` + jsFunc(t, html, "overviewCoverageBar") + `
` + jsFunc(t, html, "overviewCoverageCard") + `
` + jsFunc(t, html, "renderOverviewCoverageKPI") + `
` + jsFunc(t, html, "overviewRepoName") + `
` + jsFunc(t, html, "overviewRepoNumber") + `
` + jsFunc(t, html, "overviewRepoOpenIssueCount") + `
` + jsFunc(t, html, "overviewRepoOpenPRCount") + `
` + jsFunc(t, html, "overviewProjectFilterSpec") + `
` + jsFunc(t, html, "repoCardParkedIssues") + `
` + jsFunc(t, html, "overviewRepoTrackedIssueEntries") + `
` + jsFunc(t, html, "overviewRepoTrackedPREntries") + `
` + jsFunc(t, html, "overviewProjectBucketCount") + `
` + jsFunc(t, html, "overviewNonActionableMoved") + `
` + jsFunc(t, html, "overviewProjectOutsideIssueCount") + `
` + jsFunc(t, html, "overviewProjectOutsidePRCount") + `
` + jsFunc(t, html, "overviewProjectFilterMatchesEntry") + `
` + jsFunc(t, html, "overviewProjectFilterRepoMatches") + `
` + jsFunc(t, html, "overviewProjectFilterSummary") + `
` + jsFunc(t, html, "renderOverviewKPIs") + `
const document = { _els: {}, getElementById(id) { if (!this._els[id]) this._els[id] = { id, hidden: true, textContent: '0/0', title: '', attrs: { 'data-zero': '' }, setAttribute(k, v) { this.attrs[k] = v; }, removeAttribute(k) { delete this.attrs[k]; } }; return this._els[id]; } };
` + jsFunc(t, html, "overviewSidebarPill") + `
` + jsFunc(t, html, "overviewSidebarPillLead") + `
` + jsFunc(t, html, "overviewUnlistedCounts") + `
` + jsFunc(t, html, "overviewUnlistedNote") + `
` + jsFunc(t, html, "overviewSyncingNote") + `
` + jsFunc(t, html, "sidebarPairBadgeText") + `
` + jsFunc(t, html, "setSidebarPairBadge") + `
`
}

func TestOverviewKPIValuesRenderFromRepoTotalsOrChartSlices10139(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: Overview KPI render rule was not executed")
	}
	html := indexHTML(t)
	script := overviewKPIRenderPrelude(t, html) + `
const state = { showKPIs: true };
// Rows carry the server-stamped overview_term; the Overview counts them and
// leaves the rest of the forge counts in Outside.
const row = term => ({ overview_term: term });
const repos = [{
  issues: 100, prs: 7,
  actionableIssues: [row('actionable'), row('actionable'), row('actionable')],
  heldIssues: [{ number: 501, overview_term: 'held' }],
  nonActionableIssues: [row('your_turn'), row('waiting_elsewhere'), row('outside')],
  openPrs: [row('actionable'), row('blocked')],
  heldPrs: [row('your_turn')],
}];
_overviewLastRepos = repos;
const issueSlices = [
  { key: 'ready', count: 93, items: [{ updated_at: new Date(Date.now() - 7 * 60000).toISOString() }] },
  { key: 'in-progress', count: 4, items: [] },
  { key: 'agent-filed', count: 0, items: [] },
  { key: 'waiting', count: 0, items: [] },
  { key: 'done', count: 2, items: [] },
];
const prSlices = [
  { key: 'waiting', count: 0, items: [] },
  { key: 'eligible', count: 4, items: [] },
  { key: 'blocked', count: 1, items: [] },
  { key: 'in-review', count: 2, items: [] },
  { key: 'open', count: 0, items: [] },
  { key: 'draft', count: 0, items: [] },
];
const out = renderOverviewKPIs(repos, issueSlices, prSlices, state);
const values = [...out.matchAll(/data-overview-kpi-key="([^"]+)" data-overview-kpi-value="([^"]+)"[^>]*>([^<]*)<\/span><span class="overview-kpi-label">([^<]*)/g)].map(m => [m[4], m[3]]);
assert.equal(values.length, 8);
for (const [label, value] of values) assert.notEqual(value, '', label + ' rendered an empty KPI value');
const renderedValues = Object.fromEntries(values.map(([label, value]) => [label, Number(value)]));
assert.deepEqual(Object.fromEntries(values), {
  'Total open issues': '100',
  'Total open PRs': '7',
  'Actionable now': '4',
  'Your turn': '2',
  'Waiting on someone else': '1',
  'Held': '1',
  'Blocked': '1',
  'Outside': '98',
});
assert.equal(
  renderedValues['Total open issues'] + renderedValues['Total open PRs'],
  renderedValues['Actionable now'] + renderedValues['Your turn'] + renderedValues['Waiting on someone else'] + renderedValues.Held + renderedValues.Blocked + renderedValues.Outside
);
assert.doesNotMatch(out, /blocked\/needs-human|Blocked \/ needs-human/i);
assert.match(out, /aria-label="100 total open issues \+ 7 total open PRs = 4 actionable now \+ 2 your turn \+ 1 waiting on someone else \+ 1 held \+ 1 blocked \+ 98 outside"/);
let stored = JSON.parse(localStorage.getItem(overviewKPILocalKey()) || '[]');
assert.equal(stored.length, 1);
assert.equal(stored[0].overviewOpenIssues, 100);

repos[0].countsIncomplete = true;
repos[0].issues = 1;
repos[0].prs = 1;
renderOverviewKPIs(repos, issueSlices, prSlices, state);
stored = JSON.parse(localStorage.getItem(overviewKPILocalKey()) || '[]');
assert.equal(stored.length, 1);
assert.equal(stored[0].overviewOpenIssues, 100);
delete repos[0].countsIncomplete;

repos[0].issues = 123;
repos[0].prs = 45;
const rawOut = renderOverviewKPIs(repos, issueSlices, prSlices, state);
const rawValues = Object.fromEntries([...rawOut.matchAll(/data-overview-kpi-key="([^"]+)" data-overview-kpi-value="([^"]+)"[^>]*>([^<]*)<\/span><span class="overview-kpi-label">([^<]*)/g)].map(m => [m[4], m[3]]));
assert.equal(rawValues['Total open issues'], '123');
assert.equal(rawValues['Total open PRs'], '45');
assert.equal(rawValues['Outside'], '159');
assert.doesNotMatch(out, /Median actionable age/);

window._lastStatus.overviewCoverage = { coverage: 87, target: 91, source: 'coverage badge (HIVE_COVERAGE_BADGE_URL)', repo: 'hivecommons/hive' };
_overviewKPIHistory = [{ t: Date.now() - 600000, overviewCoverage: 86 }, { t: Date.now(), overviewCoverage: 87 }];
const coverageOut = renderOverviewKPIs(repos, issueSlices, prSlices, state);
assert.equal((coverageOut.match(/class="overview-kpi(?:\s|")/g) || []).length, 9);
assert.match(coverageOut, />87%<\/span><span class="overview-kpi-label">Test coverage/);
assert.match(coverageOut, /goal 91% · hivecommons\/hive/);
assert.match(coverageOut, /data-action="openAgentCollapsedTile"[^>]*data-agent="quality"/);
assert.match(coverageOut, /overview-kpi-coverage-spacer/);
assert.match(coverageOut, /overview-kpi:test-coverage/);
assert.match(coverageOut, /ACMM Level 3→4 readiness signal/);
_overviewKPIHistory = [];
localStorage.data = {};
const coverageBarOut = renderOverviewKPIs(repos, issueSlices, prSlices, state);
assert.match(coverageBarOut, /overview-coverage-bar/);
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node overview KPI render failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}

func TestOverviewKPIProjectFiltersShareClassificationFunction(t *testing.T) {
	html := indexHTML(t)
	render := jsFunc(t, html, "renderOverviewKPIs")
	summary := jsFunc(t, html, "overviewProjectFilterSummary")
	predicate := jsFunc(t, html, "overviewProjectFilterRepoMatches")
	apply := jsFunc(t, html, "overviewApplyKPIProjectFilter")
	setHash := jsFunc(t, html, "overviewSetProjectFilterHash")
	for _, want := range []string{
		`data-action="overviewApplyKPIProjectFilter"`,
		`data-arg0="${esc(card.key)}"`,
	} {
		if !strings.Contains(render, want) {
			t.Fatalf("renderOverviewKPIs no longer wires KPI tile counts/clicks through %q", want)
		}
	}
	if !strings.Contains(summary, "overviewProjectFilterRepoMatches(repo, filter).count") {
		t.Fatal("overviewProjectFilterSummary must count Projects matches through the shared predicate")
	}
	if !strings.Contains(predicate, "overviewProjectFilterMatchesEntry(entry, filter)") {
		t.Fatal("overviewProjectFilterRepoMatches must filter individual issue/PR rows with the shared band predicate")
	}
	if !strings.Contains(apply, "overviewSetProjectFilterHash(key)") || !strings.Contains(setHash, "#projects?band=") || !strings.Contains(html, "Filtered by: ${esc(filter.label)}") {
		t.Fatal("KPI project filters must be deep-linkable and visibly clearable")
	}
}

func TestOverviewKPIRenderEscapesNumericValues10139(t *testing.T) {
	html := indexHTML(t)
	kpis := jsFunc(t, html, "renderOverviewKPIs")
	for _, want := range []string{
		"const openIssues = Number(forgeTotals?.issues?.forge ?? issueEq.open);",
		"const openPRs = Number(forgeTotals?.prs?.forge ?? prEq.open);",
		"${esc(String(value ?? '—'))}",
	} {
		if !strings.Contains(kpis, want) {
			t.Errorf("renderOverviewKPIs missing %q", want)
		}
	}
	if regexp.MustCompile(`esc\(value\)`).FindString(kpis) != "" {
		t.Errorf("renderOverviewKPIs must not pass numeric KPI values directly to esc")
	}
}
