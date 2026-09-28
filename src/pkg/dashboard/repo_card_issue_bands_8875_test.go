package dashboard

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"
)

func TestRepoCardIssueBandsStaticWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`<div id="repos-legend" class="repo-legend"></div>`,
		"function renderRepoLegend()",
		"function groupedRepoIssues(issues)",
		"const issuePills = groupedRepoIssues((r.actionableIssues || []).concat(r.heldIssues || [])).map(g => {",
		"repoStaleIssueCount(r.actionableIssues || [])",
		"window._repoIssueBandConfig = normalizeIssueBandConfig(cfg.dashboard_issue_bands || {});",
		".repo-issue-pill.waiting",
		".repo-issue-pill.agent-filed",
		"REPO_LEGEND_COLLAPSED_KEY_PREFIX",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}

func TestRepoLegendRedesign(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		".repo-legend-v2 .repo-legend-body { display:grid;",
		".repo-legend-items { display:grid; grid-template-columns:max-content 1fr;",
		"@media (max-width:640px) { .repo-legend-v2 .repo-legend-body { grid-template-columns:1fr; } }",
		"const legendGroups = [",
		"title: 'Issues — left chip is the band'",
		"title: 'Pull requests — merge state'",
		"title: 'Review, links & actions'",
		"repo-legend-colors",
		"held (agents skip it)",
		"dimmed = no activity > ${cfg.staleDays}d",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("redesigned legend missing %q", want)
		}
	}
	for _, gone := range []string{
		"repo-legend-row\"><span class=\"repo-legend-row-label\">PR bands",
		"title=\"Hold actionable item\"",
		"title=\"Release held item\"",
		"title=\"Queue for auto-merge, or disabled when not mergeable on GitHub\"",
		"title=\"Open PR with no merge verdict\"",
		"SCANNER</span> agent",
		"aligned row</span>",
	} {
		if strings.Contains(html, gone) {
			t.Errorf("redesigned legend still contains dropped entry %q", gone)
		}
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: repository legend was not executed")
	}
	funcs := []string{
		"normalizeIssueBandConfig",
		"repoIssueBandConfig",
		"repoLegendCollapsedKey",
		"isRepoLegendCollapsed",
		"renderRepoLegend",
		"esc",
	}
	var script strings.Builder
	script.WriteString(`
const assert = require('node:assert/strict');
const REPO_ISSUE_BAND_DEFAULTS = {
  waitingLabels: ['blocked', 'needs-decision', '2-discussing', 'Epic', 'needs-human', 'needs-triage'],
  doneLabels: ['hive/already-done', 'hive/covered-by-pr', 'hive/likely-done'],
  staleDays: 14
};
const OVERVIEW_ISSUE_BAND_ORDER = ['ready', 'in-progress', 'agent-filed', 'waiting', 'done'];
const window = { _repoIssueBandConfig: { stale_days: 9 }, _lastStatus: { hiveId: 'legend-test' } };
const localStorage = { getItem: () => '0' };
const legendEl = { className: '', innerHTML: '' };
const document = { getElementById: id => id === 'repos-legend' ? legendEl : null };
function setIfChanged(el, html) { el.innerHTML = html; }
let bandCalls = [];
function issueBandSpec(band) {
  bandCalls.push(band);
  return { label: 'Label ' + band, short: 'band-' + band, rule: 'rule ' + band };
}
function issueBandTip(band) { return 'tip ' + band; }
`)
	for _, name := range funcs {
		script.WriteString(jsFunc(t, html, name))
		script.WriteByte('\n')
	}
	script.WriteString(`
renderRepoLegend();
assert.equal(legendEl.className, 'repo-legend repo-legend-v2');
const out = legendEl.innerHTML;
assert.match(out, /needs you/);
assert.match(out, /in progress \/ outstanding/);
assert.match(out, /ready \/ done/);
assert.match(out, /agent-filed/);
assert.match(out, /held \(agents skip it\)/);
assert.match(out, /🕒 dimmed = no activity &gt; 9d/);
assert.match(out, /Issues — left chip is the band/);
assert.match(out, /Pull requests — merge state/);
assert.match(out, /Review, links &amp; actions/);
assert.deepEqual(bandCalls.slice(0, OVERVIEW_ISSUE_BAND_ORDER.length), OVERVIEW_ISSUE_BAND_ORDER);
for (const band of OVERVIEW_ISSUE_BAND_ORDER) assert.match(out, new RegExp('band-' + band));
assert.match(out, /<span>nobody on it/);
assert.match(out, /⛔ blocked · ❓ needs a decision/);
assert.match(out, /on an issue: verified linked PR/);
assert.match(out, /on a PR: issue it closes on merge/);
assert.match(out, /queued for auto-merge/);
assert.equal((out.match(/Queue for auto-merge/g) || []).length, 0);
assert.equal((out.match(/Hold actionable item/g) || []).length, 0);
assert.equal((out.match(/Release held item/g) || []).length, 0);
assert.equal((out.match(/PR bands/g) || []).length, 0);
assert.equal((out.match(/SCANNER/g) || []).length, 0);
assert.equal((out.match(/aligned row/g) || []).length, 0);
`)
	if out, err := exec.Command(node, "-e", script.String()).CombinedOutput(); err != nil {
		t.Fatalf("repository legend redesign check failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}

func TestConfigEndpointExposesIssueBandTaxonomy(t *testing.T) {
	s := covApiServer(t)
	s.deps.Config.Dashboard.IssueBands.WaitingLabels = []string{"blocked", "needs-decision"}
	s.deps.Config.Dashboard.IssueBands.DoneLabels = []string{"hive/already-done"}
	s.deps.Config.Dashboard.IssueBands.StaleDays = 21
	rec := doOwnerGet(s, "/api/config")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/config: expected 200, got %d", rec.Code)
	}
	var body struct {
		Bands struct {
			WaitingLabels []string `json:"waiting_labels"`
			DoneLabels    []string `json:"done_labels"`
			StaleDays     int      `json:"stale_days"`
		} `json:"dashboard_issue_bands"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if strings.Join(body.Bands.WaitingLabels, ",") != "blocked,needs-decision" || strings.Join(body.Bands.DoneLabels, ",") != "hive/already-done" || body.Bands.StaleDays != 21 {
		t.Fatalf("dashboard_issue_bands = %+v", body.Bands)
	}
}
