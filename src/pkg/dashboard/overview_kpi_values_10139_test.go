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
` + jsFunc(t, html, "esc") + `
` + jsFunc(t, html, "overviewItemAgeMinutes") + `
` + jsFunc(t, html, "overviewMedianAgeLabel") + `
` + jsFunc(t, html, "overviewMedianAgeSeconds") + `
` + jsFunc(t, html, "fmtDurationFromSeconds") + `
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
const state = { showKPIs: true, timeBasis: 'updated' };
const repos = [{ heldIssues: [{ number: 501 }], heldPrs: [] }];
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
assert.equal(values.length, 6);
for (const [label, value] of values) assert.notEqual(value, '', label + ' rendered an empty KPI value');
assert.deepEqual(Object.fromEntries(values), {
  'Total open issues': '99',
  'Total open PRs': '7',
  'Actionable now (issues and PRs)': '103',
  'Held': '1',
  'Blocked / needs-human': '1',
  'Median actionable age (updated)': '7m',
});

repos[0].issues = 123;
repos[0].prs = 45;
const rawOut = renderOverviewKPIs(repos, issueSlices, prSlices, state);
const rawValues = Object.fromEntries([...rawOut.matchAll(/<span class="overview-kpi-value"[^>]*>([^<]*)<\/span><span class="overview-kpi-label">([^<]*)<\/span>/g)].map(m => [m[2], m[1]]));
assert.equal(rawValues['Total open issues'], '123');
assert.equal(rawValues['Total open PRs'], '45');
assert.match(out, /title="Median time since updated for actionable open issues and PRs in the selected repos \(not MTTR, which measures time-to-resolve for closed items\)\."/);
const createdOut = renderOverviewKPIs(repos, issueSlices, prSlices, { showKPIs: true, timeBasis: 'created' });
assert.match(createdOut, />7m<\/span><span class="overview-kpi-label">Median actionable age \(created\)<\/span>/);
assert.match(createdOut, /title="Median time since created for actionable open issues and PRs in the selected repos \(not MTTR, which measures time-to-resolve for closed items\)\."/);
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
		"const openIssues = repoIssueCounts.length ? repoIssueCounts.reduce((n, count) => n + count, 0) : (issueSlices || []).reduce((n, s) => n + Number(s.count || 0), 0);",
		"const openPRs = repoPRCounts.length ? repoPRCounts.reduce((n, count) => n + count, 0) : (prSlices || []).reduce((n, s) => n + Number(s.count || 0), 0);",
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
