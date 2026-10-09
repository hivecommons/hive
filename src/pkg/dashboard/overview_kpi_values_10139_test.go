package dashboard

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func TestOverviewKPIValuesRenderFromRepoTotalsOrChartSlices10139(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: Overview KPI render rule was not executed")
	}
	html := indexHTML(t)
	script := `const assert = require('node:assert/strict');
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
  'open-issues': { label: 'Total open issues', kinds: ['issue'], issueBands: OVERVIEW_ISSUE_BAND_ORDER, bucketKind: 'issues' },
  'open-prs': { label: 'Total open PRs', kinds: ['pr'], prBands: PR_BAND_ORDER, bucketKind: 'prs' },
  'actionable-now': { label: 'Actionable now', kinds: ['issue', 'pr'], issueBands: OVERVIEW_ISSUE_BAND_ORDER.filter(b => !['waiting', 'done'].includes(b)), prBands: PR_BAND_ORDER.filter(b => !['waiting', 'draft', 'blocked'].includes(b)) },
  'held': { label: 'Held', kinds: ['issue', 'pr'], held: true },
  'blocked-needs-human': { label: 'Blocked / needs-human', kinds: ['issue', 'pr'], issueBands: ['waiting', 'done'], prBands: ['waiting', 'blocked'], issueBuckets: ['needs_human'] },
  'outside': { label: 'Outside', kinds: ['issue', 'pr'], outside: true }
};
let _overviewProjectFilterKey = null;
let _overviewProjectFilterCount = null;
function repoItemNeedsHuman(item){ return ((item && item.labels) || []).some(l => String(l).toLowerCase() === 'needs-human'); }
const localStorage = { data: {}, getItem(k){ return Object.prototype.hasOwnProperty.call(this.data,k) ? this.data[k] : null; }, setItem(k,v){ this.data[k]=String(v); }, removeItem(k){ delete this.data[k]; } };
function fmtSparkVal(v){ return String(v); }
function renderSparkline(){ return '<svg></svg>'; }
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
` + jsFunc(t, html, "renderOverviewTotalPartitionSubline") + `
` + jsFunc(t, html, "renderOverviewSplitSubline") + `
` + jsFunc(t, html, "overviewKPIEquation") + `
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
` + jsFunc(t, html, "overviewRepoName") + `
` + jsFunc(t, html, "overviewRepoNumber") + `
` + jsFunc(t, html, "overviewRepoOpenIssueCount") + `
` + jsFunc(t, html, "overviewRepoOpenPRCount") + `
` + jsFunc(t, html, "overviewProjectFilterSpec") + `
` + jsFunc(t, html, "overviewRepoTrackedIssueEntries") + `
` + jsFunc(t, html, "overviewRepoTrackedPREntries") + `
` + jsFunc(t, html, "overviewProjectBucketCount") + `
` + jsFunc(t, html, "overviewProjectOutsideIssueCount") + `
` + jsFunc(t, html, "overviewProjectOutsidePRCount") + `
` + jsFunc(t, html, "overviewProjectFilterMatchesEntry") + `
` + jsFunc(t, html, "overviewProjectFilterRepoMatches") + `
` + jsFunc(t, html, "overviewProjectFilterSummary") + `
` + jsFunc(t, html, "renderOverviewKPIs") + `
const state = { showKPIs: true };
const repos = [{ issues: 100, prs: 7, heldIssues: [{ number: 501 }], heldPrs: [] }];
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
assert.equal(values.length, 6);
for (const [label, value] of values) assert.notEqual(value, '', label + ' rendered an empty KPI value');
const renderedValues = Object.fromEntries(values.map(([label, value]) => [label, Number(value)]));
assert.deepEqual(Object.fromEntries(values), {
  'Total open issues': '100',
  'Total open PRs': '7',
  'Actionable now': '103',
  'Held': '1',
  'Blocked / needs-human': '3',
  'Outside': '0',
});
assert.equal(
  renderedValues['Total open issues'] + renderedValues['Total open PRs'],
  renderedValues['Actionable now'] + renderedValues.Held + renderedValues['Blocked / needs-human'] + renderedValues.Outside
);
assert.match(out, /aria-label="100 total open issues \+ 7 total open PRs = 103 actionable now \+ 1 held \+ 3 blocked or needs-human \+ 0 outside"/);
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
assert.equal(rawValues['Outside'], '61');
assert.doesNotMatch(out, /Median actionable age/);
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
		"overviewProjectFilterSummary(key, repos, issueSlices, prSlices)",
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
		"const trackedIssues = (issueSlices || []).reduce((n, s) => n + Number(s.count || 0), 0);",
		"const trackedPRs = (prSlices || []).reduce((n, s) => n + Number(s.count || 0), 0);",
		"const openIssues = Number(forgeTotals?.issues?.forge ?? trackedIssues);",
		"const openPRs = Number(forgeTotals?.prs?.forge ?? trackedPRs);",
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
