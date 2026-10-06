package dashboard

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

func TestOverviewKPIHistoryAppendCapPersistRestoreDownsample(t *testing.T) {
	s := &Server{}
	start := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	status := &StatusPayload{
		Hold: FrontendHold{Total: 1},
		Repos: []FrontendRepo{{
			Name:   "hive",
			Issues: 3, PRs: 1,
			ActionableIssues: []any{github.Issue{CreatedAt: start.Add(-2 * time.Hour), UpdatedAt: start.Add(-1 * time.Hour)}},
			HeldIssues:       []any{github.HoldItem{Number: 2, Type: "issue", Labels: []string{"hold"}}},
			OpenPrs:          []any{github.PullRequest{CreatedAt: start.Add(-3 * time.Hour), UpdatedAt: start.Add(-30 * time.Minute), Labels: []string{"needs-human"}}},
			HeldPrs:          []any{github.PullRequest{Labels: []string{"needs-human"}}},
		}},
	}
	for i := 0; i < trendHistoryMaxEntries+5; i++ {
		s.appendTrendHistoryAt(status, start.Add(time.Duration(i)*timeHistoryStep()))
	}
	got := s.TrendHistory()
	if len(got) != trendHistoryMaxEntries {
		t.Fatalf("history len = %d, want cap %d", len(got), trendHistoryMaxEntries)
	}
	last := got[len(got)-1]
	if last.OverviewOpenIssues != 3 || last.OverviewOpenPRs != 1 || last.OverviewActionable != 1 || last.OverviewHeld != 2 || last.OverviewBlockedHuman != 1 || last.OverviewOutside != 0 {
		t.Fatalf("overview KPI sample = %+v", last)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "overview-history.json")
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	restoredData, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var restored []TrendHistoryEntry
	if err := json.Unmarshal(restoredData, &restored); err != nil {
		t.Fatal(err)
	}
	restoredServer := &Server{}
	restoredServer.SeedTrendHistory(restored)
	downsampled := restoredServer.OverviewKPIHistory(got[len(got)-200].Timestamp)
	if len(downsampled) > overviewKPIHistoryMaxPoints {
		t.Fatalf("downsampled len = %d, want <= %d", len(downsampled), overviewKPIHistoryMaxPoints)
	}
	if downsampled[0].Timestamp != got[len(got)-200].Timestamp {
		t.Fatalf("downsample kept first timestamp %d, want %d", downsampled[0].Timestamp, got[len(got)-200].Timestamp)
	}
	if downsampled[len(downsampled)-1].Timestamp != got[len(got)-1].Timestamp {
		t.Fatalf("downsample kept last timestamp %d, want %d", downsampled[len(downsampled)-1].Timestamp, got[len(got)-1].Timestamp)
	}
}

func TestOverviewKPIHistoryJSONIncludesZeroOverviewFields(t *testing.T) {
	s := &Server{}
	s.appendTrendHistoryAt(&StatusPayload{}, time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC))
	data, err := json.Marshal(s.OverviewKPIHistory(0))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		"overviewOpenIssues",
		"overviewOpenPrs",
		"overviewActionable",
		"overviewHeld",
		"overviewBlockedHuman",
		"overviewOutside",
	} {
		if !strings.Contains(string(data), `"`+field+`":0`) {
			t.Fatalf("marshaled zero-value overview history missing %s: %s", field, data)
		}
	}
	if strings.Contains(string(data), `"overviewMedianAgeSec"`) {
		t.Fatalf("marshaled overview history reported unavailable median as zero: %s", data)
	}
}

func timeHistoryStep() time.Duration {
	return time.Duration(trendHistoryMinIntervalMs) * time.Millisecond
}

func TestOverviewKPITilesRenderSparklines(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: overview KPI rendering behavior was not executed")
	}
	html := indexHTML(t)
	var source strings.Builder
	source.WriteString(`const assert = require('node:assert/strict');
function esc(s){return String(s ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));}
const escapeHtml = esc, SPARK_W = 80, SPARK_H = 20;
const OVERVIEW_KPI_WINDOWS = {'24h': 86400000, '7d': 604800000};
const OVERVIEW_REPOS_KEY = 'hive.overview.repos';
let _overviewKPIWindow = '24h';
let _overviewKPILocalScope = 'all';
let _overviewLastRepos = [{name: 'hive', full: 'hivecommons/hive'}];
global.window = {_lastStatus: {hiveId: 'test'}};
global.localStorage = { getItem(){ return null; }, setItem(){}, removeItem(){} };
let _overviewKPIHistory = [
  {t: Date.now(), overviewOpenIssues: 58, overviewOpenPrs: 12, overviewActionable: 36, overviewHeld: 6, overviewBlockedHuman: 3, overviewOutside: 25}
];
const OVERVIEW_ISSUE_BREAKDOWN_LABELS = { needs_human: 'needs-human', needs_direction: 'needs-direction', needs_decision: 'needs-decision', needs_spec: 'needs-spec', exempt: 'exempt', filtered: 'filtered', reporter_triage: 'reporter triage', hive_advisory: 'hive advisory', dependency_dashboard: 'dependency dashboard', other: 'other' };
const OVERVIEW_PR_BREAKDOWN_LABELS = { hold: 'held', draft: 'draft', filtered: 'filtered', other: 'other' };
function overviewKPILoadLocal(){ return []; }
function overviewKPIRecordLocal(){}
function overviewKPIWindowControls(){ return '<span class="overview-kpi-range"></span>'; }
function overviewItemAgeMinutes(){ return NaN; }
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
`)
	for _, name := range []string{
		"fmtSparkVal", "sparklineSeriesKey", "sparklineReducedMotion", "sparklineValueSummary", "renderSparkline",
		"overviewKPIRepoScope", "overviewKPIHistoryEntries",
		"overviewRepoName", "overviewAllRepoNames", "overviewSavedRepoNames", "overviewSelectedRepoNames", "overviewFilterRepos",
		"overviewRepoNumber", "overviewRepoOpenIssueCount", "overviewRepoOpenPRCount",
		"overviewBreakdownTotal", "overviewRepoForgeTotals", "overviewKPIForgeTotals", "overviewKPIBreakdownSubline",
		"overviewKPITerm", "dashboardDocsHref", "dashboardDocsHrefFromPath", "overviewPartitionDocsLink", "overviewPartitionSettingsButton", "overviewPartitionSummaryLine", "overviewPartitionGloss", "overviewPartitionPreferredDocs", "overviewPartitionPreferredSettings", "overviewPartitionRowHTML", "overviewPartitionTooltip", "overviewPartitionInfo", "overviewActionableTermHTML", "renderActionableEquationSubline",
		"overviewEquationTermCount", "overviewOutsideBreakdownText", "renderOverviewTotalPartitionSubline", "renderOverviewSplitSubline",
		"overviewKPIEquation", "overviewKPIKindEquation", "overviewKPISparkTitle", "overviewKPISpark", "overviewKPICurrentSample",
		"overviewProjectFilterSpec", "overviewRepoTrackedIssueEntries", "overviewRepoTrackedPREntries", "overviewProjectBucketCount",
		"overviewProjectOutsideIssueCount", "overviewProjectOutsidePRCount", "overviewProjectFilterMatchesEntry", "overviewProjectFilterRepoMatches",
		"overviewProjectFilterSummary", "renderOverviewKPIs",
	} {
		source.WriteString(jsFunc(t, html, name))
		source.WriteByte('\n')
	}
	source.WriteString(`
const issueSlices = [{key: 'ready', count: 19, items: []}];
const prSlices = [{key: 'ready', count: 12, items: []}, {key: 'blocked', count: 3, items: []}];
// Match the production renderOverviewCharts path: _overviewLastRepos is the
// full status payload repo shape (name + full) and renderOverviewKPIs receives
// the selected objects returned by overviewFilterRepos.
const liveRepos = [{name: 'hive', full: 'hivecommons/hive', issues: 39, prs: 31, actionableIssues: [1,2,3,4,5,6,7], heldIssues: [8,9], openPrs: [10,11], heldPrs: [12,13]}];
_overviewLastRepos = liveRepos;
const markup = renderOverviewKPIs(overviewFilterRepos(liveRepos), issueSlices, prSlices, {showKPIs: true});
assert.equal((markup.match(/class="overview-kpi"/g) || []).length, 6);
assert.equal((markup.match(/<svg/g) || []).length, 6);
assert.match(markup, />39<\/span><span class="overview-kpi-label">Total open issues<\/span>/);
assert.match(markup, />31<\/span><span class="overview-kpi-label">Total open PRs<\/span>/);
assert.match(markup, /aria-label="39 total open issues \+ 31 total open PRs = /);
for (const key of ['open-issues','open-prs','actionable-now','held','blocked-needs-human','outside']) {
  assert.match(markup, new RegExp('overview-kpi:' + key));
}
`)
	if out, err := exec.Command(node, "-e", source.String()).CombinedOutput(); err != nil {
		t.Fatalf("overview KPI renderer failed: %v\n%s", err, out)
	}
}
