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
		Hold:             FrontendHold{Total: 1},
		OverviewCoverage: &FrontendOverviewCoverage{Coverage: 87, Target: coverageTarget, Source: qualityCoverageSource, Repo: "hivecommons/hive"},
		Repos: []FrontendRepo{{
			Name:   "hive",
			Issues: 3, PRs: 1,
			ActionableIssues: []any{github.Issue{CreatedAt: start.Add(-2 * time.Hour), UpdatedAt: start.Add(-1 * time.Hour)}},
			HeldIssues:       []any{github.HoldItem{Number: 2, Type: "issue", Labels: []string{"hold"}}},
			OpenPrs:          []any{github.PullRequest{Number: 3, CreatedAt: start.Add(-3 * time.Hour), UpdatedAt: start.Add(-30 * time.Minute), Labels: []string{"needs-human"}}},
			HeldPrs:          []any{github.PullRequest{Number: 4, Labels: []string{"needs-human"}}},
			Ownership: map[string]Ownership{
				ownershipKey(ownershipKindPR, 3): yourTurn(OwnershipReasonPersonPRNeedsHuman, "Needs human review"),
				ownershipKey(ownershipKindPR, 4): yourTurn(OwnershipReasonLevelGateHold, "Held by the level gate"),
			},
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
	if last.OverviewOpenIssues != 3 || last.OverviewOpenPRs != 1 || last.OverviewActionable != 1 || last.OverviewHeld != 1 || last.OverviewYourTurn != 2 || last.OverviewWaiting != 0 || last.OverviewBlocked != 0 || last.OverviewOutside != 0 || !last.OverviewCoverageOK || last.OverviewCoverage != 87 {
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
	if downsampled[len(downsampled)-1].OverviewCoverage == nil || *downsampled[len(downsampled)-1].OverviewCoverage != 87 {
		t.Fatalf("downsampled overview coverage = %+v, want 87", downsampled[len(downsampled)-1].OverviewCoverage)
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
		"overviewYourTurn",
		"overviewWaiting",
		"overviewBlocked",
		"overviewOutside",
	} {
		if !strings.Contains(string(data), `"`+field+`":0`) {
			t.Fatalf("marshaled zero-value overview history missing %s: %s", field, data)
		}
	}
	if strings.Contains(string(data), `"overviewMedianAgeSec"`) {
		t.Fatalf("marshaled overview history reported unavailable median as zero: %s", data)
	}
	if strings.Contains(string(data), `"overviewCoverage"`) {
		t.Fatalf("marshaled overview history must omit unavailable coverage: %s", data)
	}
}

func TestOverviewKPIHistorySkipsIncompleteSamples(t *testing.T) {
	s := &Server{}
	start := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	complete := &StatusPayload{Repos: []FrontendRepo{{Name: "hive", Issues: 33, PRs: 14}}}
	incomplete := &StatusPayload{
		OverviewKPIIncomplete: true,
		Repos:                 []FrontendRepo{{Name: "hive", Issues: 2, PRs: 1, CountsIncomplete: true}},
	}

	s.appendTrendHistoryAt(complete, start)
	s.appendTrendHistoryAt(incomplete, start.Add(timeHistoryStep()))
	s.appendTrendHistoryAt(complete, start.Add(2*timeHistoryStep()))

	history := s.OverviewKPIHistory(0)
	if len(history) != 3 {
		t.Fatalf("history len = %d, want 3 trend rows", len(history))
	}
	if history[1].OverviewOpenIssues != nil || history[1].OverviewOpenPRs != nil {
		t.Fatalf("incomplete sample should omit KPI values, got %+v", history[1])
	}
	values := make([]int, 0, 2)
	for _, row := range history {
		if row.OverviewOpenIssues != nil {
			values = append(values, *row.OverviewOpenIssues)
		}
	}
	if len(values) != 2 || values[0] != 33 || values[1] != 33 {
		t.Fatalf("persisted overview issue values = %v, want flat last-good samples", values)
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
global.document = { getElementById(){ return null; } };
function relativeTimeShort(){ return ''; }
let _overviewKPIHistory = [
  {t: Date.now(), overviewOpenIssues: 58, overviewOpenPrs: 12, overviewActionable: 36, overviewHeld: 6, overviewYourTurn: 3, overviewWaiting: 2, overviewBlocked: 1, overviewOutside: 22}
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
`)
	for _, name := range []string{
		"fmtSparkVal", "sparklineSeriesKey", "sparklineReducedMotion", "sparklineValueSummary", "renderSparkline",
		"overviewKPIRepoScope", "overviewKPIHistoryEntries",
		"overviewRepoName", "overviewAllRepoNames", "overviewSavedRepoNames", "overviewSelectedRepoNames", "overviewFilterRepos",
		"overviewRepoNumber", "overviewRepoOpenIssueCount", "overviewRepoOpenPRCount",
		"overviewBreakdownTotal", "overviewRepoForgeTotals", "overviewKPIForgeTotals", "overviewKPIBreakdownSubline",
		"overviewKPITerm", "dashboardDocsHref", "dashboardDocsHrefFromPath", "overviewPartitionDocsLink", "overviewPartitionSettingsButton", "overviewPartitionSummaryLine", "overviewPartitionGloss", "overviewPartitionPreferredDocs", "overviewPartitionPreferredSettings", "overviewPartitionRowHTML", "overviewPartitionTooltip", "overviewPartitionInfo", "overviewActionableTermHTML", "renderActionableEquationSubline",
		"overviewEquationTermCount", "overviewOutsideBreakdownText", "overviewStaleSince", "fmtOverviewStaleSince", "renderOverviewTotalPartitionSubline", "renderOverviewSplitSubline",
		"overviewEquationTermDefs", "overviewKPICombinedEquation", "overviewKPILocalEquations", "overviewKPIKindEquation", "overviewKPISparkTitle", "overviewKPISpark", "overviewKPICurrentSample",
		"overviewCoverageInfo", "overviewCoverageState", "overviewCoverageColor", "overviewCoverageInfoHTML", "overviewCoverageBar", "overviewCoverageCard", "renderOverviewCoverageKPI",
		"overviewProjectFilterSpec", "repoCardParkedIssues", "overviewRepoTrackedIssueEntries", "overviewRepoTrackedPREntries", "overviewProjectBucketCount",
		"overviewNonActionableMoved", "overviewProjectOutsideIssueCount", "overviewProjectOutsidePRCount", "overviewProjectFilterMatchesEntry", "overviewProjectFilterRepoMatches",
		"overviewProjectFilterSummary", "overviewSidebarPill", "overviewSidebarPillLead", "overviewUnlistedCounts", "overviewUnlistedNote", "overviewSyncingNote", "sidebarPairBadgeText", "setSidebarPairBadge", "renderOverviewKPIs",
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
assert.equal((markup.match(/class="overview-kpi"/g) || []).length, 8);
assert.equal((markup.match(/<svg/g) || []).length, 8);
assert.match(markup, />39<\/span><span class="overview-kpi-label">Total open issues<\/span>/);
assert.match(markup, />31<\/span><span class="overview-kpi-label">Total open PRs<\/span>/);
assert.match(markup, /aria-label="39 total open issues \+ 31 total open PRs = /);
for (const key of ['open-issues','open-prs','actionable-now','your-turn','waiting-elsewhere','held','blocked','outside']) {
  assert.match(markup, new RegExp('overview-kpi:' + key));
}
`)
	if out, err := exec.Command(node, "-e", source.String()).CombinedOutput(); err != nil {
		t.Fatalf("overview KPI renderer failed: %v\n%s", err, out)
	}
}

func TestOverviewKPISampleMatchesTileTerms(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	status := &StatusPayload{Repos: []FrontendRepo{{
		Name: "hive", Full: "hivecommons/hive",
		Issues: 3,
		ActionableIssues: []any{
			github.Issue{Repo: "hivecommons/hive", Number: 1, Title: "confirm", Labels: []string{"hive/likely-done"}},
		},
		NonActionableIssues: []any{
			github.RepoWorkIssue{Repo: "hivecommons/hive", Number: 2, Labels: []string{"needs-decision"}, Bucket: repoWorkBucketFiltered, Reason: "needs-decision"},
			github.RepoWorkIssue{Repo: "hivecommons/hive", Number: 3, Labels: []string{"needs-reporter-confirmation"}, Bucket: repoWorkBucketFiltered, Reason: "needs-reporter-confirmation"},
		},
		Ownership: map[string]Ownership{
			ownershipKey(ownershipKindIssue, 2): yourTurn(OwnershipReasonParkedNeedsDecision, "Parked until a maintainer decides"),
			ownershipKey(ownershipKindIssue, 3): waitingElsewhere(),
		},
		WorkBreakdown: &github.RepoWorkBreakdown{
			Issues: github.RepoIssueBreakdown{Actionable: 1, Filtered: 2, NeedsDecision: 1, ReporterConfirmation: 1},
		},
	}}}
	s := &Server{}
	tile := s.statusWithOverviewBands(status, now).ActionableNow
	var e TrendHistoryEntry
	s.attachOverviewKPI(&e, status, now)
	for name, pair := range map[string][2]int{
		"your turn":  {e.OverviewYourTurn, overviewTermCount(tile.Equation, "your_turn")},
		"waiting":    {e.OverviewWaiting, overviewTermCount(tile.Equation, "waiting_elsewhere")},
		"held":       {e.OverviewHeld, overviewTermCount(tile.Equation, "held")},
		"blocked":    {e.OverviewBlocked, overviewTermCount(tile.Equation, "blocked")},
		"outside":    {e.OverviewOutside, overviewTermCount(tile.Equation, "outside")},
		"actionable": {e.OverviewActionable, tile.Total},
	} {
		if pair[0] != pair[1] {
			t.Fatalf("%s: sample = %d, tile = %d (%+v)", name, pair[0], pair[1], tile.Equation)
		}
	}
	if e.OverviewYourTurn != 1 || e.OverviewWaiting != 1 || e.OverviewActionable != 1 {
		t.Fatalf("sample = %+v, want 1 your turn, 1 waiting, 1 actionable", e)
	}
}
