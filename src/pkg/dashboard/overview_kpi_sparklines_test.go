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
			Issues: 2, PRs: 1,
			ActionableIssues: []any{github.Issue{CreatedAt: start.Add(-2 * time.Hour), UpdatedAt: start.Add(-1 * time.Hour)}},
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
	if last.OverviewOpenIssues != 2 || last.OverviewOpenPRs != 1 || last.OverviewActionable != 1 || last.OverviewHeld != 1 || last.OverviewBlockedHuman != 2 {
		t.Fatalf("overview KPI sample = %+v", last)
	}
	lastAt := start.Add(time.Duration(trendHistoryMaxEntries+4) * timeHistoryStep())
	wantMedianAgeSec := int(lastAt.Sub(start.Add(-1 * time.Hour)).Seconds())
	if last.OverviewMedianAgeSec != wantMedianAgeSec {
		t.Fatalf("median age sec = %d, want %d", last.OverviewMedianAgeSec, wantMedianAgeSec)
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
let _overviewKPIWindow = '24h';
let _overviewKPILocalScope = 'all';
let _overviewLastRepos = [{name: 'hive'}];
global.window = {_lastStatus: {hiveId: 'test'}};
let _overviewKPIHistory = [
  {t: Date.now() - 3600000, overviewOpenIssues: 50, overviewOpenPrs: 10, overviewActionable: 30, overviewHeld: 4, overviewBlockedHuman: 2, overviewMedianAgeSec: 1800},
  {t: Date.now(), overviewOpenIssues: 58, overviewOpenPrs: 12, overviewActionable: 36, overviewHeld: 6, overviewBlockedHuman: 3, overviewMedianAgeSec: 3600}
];
function overviewKPILoadLocal(){ return []; }
function overviewKPIRecordLocal(){}
function overviewKPIWindowControls(){ return '<span class="overview-kpi-range"></span>'; }
function overviewItemAgeMinutes(){ return NaN; }
`)
	for _, name := range []string{
		"fmtSparkVal", "sparklineSeriesKey", "sparklineReducedMotion", "sparklineValueSummary", "renderSparkline",
		"fmtDurationFromSeconds", "overviewMedianAgeSeconds", "overviewMedianAgeLabel", "overviewKPIRepoScope", "overviewKPIHistoryEntries",
		"overviewKPISparkTitle", "overviewKPISpark", "overviewKPICurrentSample", "renderOverviewKPIs",
	} {
		source.WriteString(jsFunc(t, html, name))
		source.WriteByte('\n')
	}
	source.WriteString(`
const issueSlices = [{key: 'ready', count: 58, items: []}];
const prSlices = [{key: 'ready', count: 12, items: []}, {key: 'blocked', count: 3, items: []}];
const markup = renderOverviewKPIs([{name: 'hive', heldIssues: [1,2], heldPrs: [3,4,5,6]}], issueSlices, prSlices, {showKPIs: true, timeBasis: 'updated'});
assert.equal((markup.match(/class="overview-kpi"/g) || []).length, 6);
assert.equal((markup.match(/<svg/g) || []).length, 6);
for (const key of ['open-issues','open-prs','actionable-now','held','blocked-needs-human','median-age']) {
  assert.match(markup, new RegExp('overview-kpi:' + key));
}
`)
	if out, err := exec.Command(node, "-e", source.String()).CombinedOutput(); err != nil {
		t.Fatalf("overview KPI renderer failed: %v\n%s", err, out)
	}
}
