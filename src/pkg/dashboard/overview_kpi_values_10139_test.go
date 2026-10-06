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
` + jsFunc(t, html, "overviewRepoNumber") + `
` + jsFunc(t, html, "overviewRepoOpenIssueCount") + `
` + jsFunc(t, html, "overviewRepoOpenPRCount") + `
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
const values = [...out.matchAll(/<span class="overview-kpi-value"[^>]*>([^<]*)<\/span><span class="overview-kpi-label">([^<]*)<\/span>/g)].map(m => [m[2], m[1]]);
assert.equal(values.length, 5);
for (const [label, value] of values) assert.notEqual(value, '', label + ' rendered an empty KPI value');
assert.deepEqual(Object.fromEntries(values), {
  'Total open issues': '100',
  'Total open PRs': '7',
  'Actionable now (issues and PRs)': '103',
  'Held': '1',
  'Blocked / needs-human': '3',
});
assert.match(out, />100 issues \+ 7 PRs = 103 actionable \+ [\s\S]*1 held[\s\S]* \+ [\s\S]*3 blocked\/needs-human</);

repos[0].issues = 123;
repos[0].prs = 45;
const rawOut = renderOverviewKPIs(repos, issueSlices, prSlices, state);
const rawValues = Object.fromEntries([...rawOut.matchAll(/<span class="overview-kpi-value"[^>]*>([^<]*)<\/span><span class="overview-kpi-label">([^<]*)<\/span>/g)].map(m => [m[2], m[1]]));
assert.equal(rawValues['Total open issues'], '123');
assert.equal(rawValues['Total open PRs'], '45');
assert.doesNotMatch(out, /Median actionable age/);
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node overview KPI render failed: %v\n%s", err, strings.TrimSpace(string(out)))
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
