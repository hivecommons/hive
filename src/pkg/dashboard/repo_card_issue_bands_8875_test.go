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
		"function groupedRepoNonActionableIssues(issues)",
		"function repoCardIssueGroups(repo, overviewIssueKeys)",
		"function repoCardIssueRenderedCount(groups)",
		"const repoIssueGroups = repoCardIssueGroups(r, overviewIssueKeys);",
		"${r.issues >= 0 ? r.issues : '?'}",
		"const issuePills = repoIssueGroups.issueGroups.map(g => {",
		"const nonActionableIssuePills = repoIssueGroups.nonActionableIssueGroups.map(g => {",
		"repoNonActionableIssueBucketSpec(i).key",
		"bucket === 'claimed_by_pr'",
		"Claimed by PR",
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
function repoPillFilterAttrs() { return ''; }
function repoPillFilterState() { return { mode: 'any', kinds: [] }; }
function repoPillFilterActive() { return false; }
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
assert.match(out, /repo-pr-pill sentinel-flagged/);
assert.match(out, /sentinel flagged it/);
assert.match(out, /trusted-author PRs/);
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

func TestRepoCardIssueGroupsCoverHeaderIssueTotal(t *testing.T) {
	html := indexHTML(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: repo-card issue grouping behavior was not executed")
	}
	funcs := []string{
		"repoItemNeedsHuman",
		"repoParkedIssueSpecs",
		"repoParkedIssueReason",
		"gHasNeedsHuman",
		"issueBandRank",
		"issueUpdatedAt",
		"groupedRepoIssues",
		"repoNonActionableIssueBucketSpec",
		"groupedRepoNonActionableIssues",
		"repoCardIssueGroups",
		"repoCardIssueRenderedCount",
	}
	var script strings.Builder
	script.WriteString(`
const assert = require('node:assert/strict');
const window = { _lastStatus: { overview_bands: { issues: [
  { key: 'ready', label: 'Ready', short: 'ready', rule: 'ready work' },
  { key: 'in-progress', label: 'In progress', short: 'claimed', rule: 'claimed work' },
  { key: 'waiting', label: 'Needs human', short: 'human', rule: 'human gate' },
  { key: 'agent-filed', label: 'Agent filed', short: 'agent', rule: 'agent filed' },
  { key: 'done', label: 'Done', short: 'done', rule: 'done' }
] } } };
function issueBandSpec(band) {
  return window._lastStatus.overview_bands.issues.find(spec => spec.key === band) || { label: band, short: band, rule: '' };
}
function issueBandLabel(band) { return issueBandSpec(band).label; }
function issueBandTip(band) { const spec = issueBandSpec(band); return spec.label + ': ' + spec.rule; }
`)
	for _, name := range funcs {
		script.WriteString(jsFunc(t, html, name))
		script.WriteByte('\n')
	}
	script.WriteString(`
const repo = {
  issues: 14,
  workBreakdown: { issues: { actionable: 7, hold: 2, hive_advisory: 1, filtered: 3, reporter_triage: 1 } },
  actionableIssues: [
    { number: 1, title: 'ready one', band: 'ready', updated_at: '2026-01-01T00:00:00Z' },
    { number: 2, title: 'claimed one', band: 'in-progress', assignees: ['bot'], updated_at: '2026-01-02T00:00:00Z' },
    { number: 3, title: 'agent filed', band: 'agent-filed', updated_at: '2026-01-03T00:00:00Z' },
    { number: 4, title: 'needs human', band: 'waiting', labels: ['needs-human'], updated_at: '2026-01-04T00:00:00Z' },
    { number: 5, title: 'done', band: 'done', labels: ['hive/covered-by-pr'], updated_at: '2026-01-05T00:00:00Z' }
  ],
  heldIssues: [
    { number: 6, title: 'held ready', band: 'ready', labels: ['hold'], updated_at: '2026-01-06T00:00:00Z' },
    { number: 7, title: 'held claimed', band: 'in-progress', labels: ['hold'], updated_at: '2026-01-07T00:00:00Z' }
  ],
  nonActionableIssues: [
    { number: 8, title: 'advisory', bucket: 'hive_advisory', updated_at: '2026-01-08T00:00:00Z' },
    { number: 9, title: 'filtered one', bucket: 'filtered', updated_at: '2026-01-09T00:00:00Z' },
    { number: 10, title: 'filtered two', bucket: 'filtered', updated_at: '2026-01-10T00:00:00Z' },
    { number: 11, title: 'filtered three', bucket: 'filtered', updated_at: '2026-01-11T00:00:00Z' },
    { number: 12, title: 'triage', bucket: 'reporter_triage', updated_at: '2026-01-12T00:00:00Z' },
    { number: 13, title: 'claimed by pr one', bucket: 'claimed_by_pr', reason: 'Open hive-authored PR #130 already claims this issue', updated_at: '2026-01-13T00:00:00Z' },
    { number: 14, title: 'claimed by pr two', bucket: 'claimed_by_pr', reason: 'Open hive-authored PR #131 already claims this issue', updated_at: '2026-01-14T00:00:00Z' }
  ]
};
const groups = repoCardIssueGroups(repo, null);
const headerTotal = Object.values(repo.workBreakdown.issues).reduce((n, v) => n + Number(v || 0), 0);
assert.equal(headerTotal, repo.issues);
assert.equal(repoCardIssueRenderedCount(groups), headerTotal);
assert.deepEqual(groups.issueGroups.map(g => [g.band, g.issues.length]), [['waiting', 1], ['ready', 2], ['in-progress', 2], ['agent-filed', 1], ['done', 1]]);
assert.deepEqual(groups.nonActionableIssueGroups.map(g => [g.key, g.issues.length]), [['hive_advisory', 1], ['filtered', 3], ['reporter_triage', 1], ['claimed_by_pr', 2]]);
for (const [reason, key, short, waiting] of [
  ['needs-human', 'needs_human', 'needs human', true],
  ['needs-decision', 'needs_decision', 'needs decision', true],
  ['needs-direction', 'needs_direction', 'needs direction', true],
  ['needs-spec', 'needs_spec', 'needs spec', true],
  ['needs-reporter-confirmation', 'reporter_confirmation', 'waiting on reporter', true],
  ['Exempt label', 'filtered', 'filtered', undefined],
  ['Project issue filter', 'filtered', 'filtered', undefined],
]) {
  const spec = repoNonActionableIssueBucketSpec({ bucket: 'filtered', reason });
  assert.equal(spec.key, key, reason);
  assert.equal(spec.short, short, reason);
  assert.equal(spec.waiting, waiting, reason);
}
assert.equal(repoNonActionableIssueBucketSpec({ bucket: 'filtered', labels: ['needs-spec'] }).key, 'needs_spec');
const seen = new Set();
for (const group of groups.issueGroups) for (const entry of group.issues) assert.equal(seen.has(entry.issue.number), false), seen.add(entry.issue.number);
for (const group of groups.nonActionableIssueGroups) for (const issue of group.issues) assert.equal(seen.has(issue.number), false), seen.add(issue.number);
assert.equal(seen.size, headerTotal);
`)
	if out, err := exec.Command(node, "-e", script.String()).CombinedOutput(); err != nil {
		t.Fatalf("repo-card issue grouping check failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}

func TestRepoCardPillColumnsScrollLongLists(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		".repo-pill-col { display: flex; flex-direction: column; gap: var(--sp-2); min-width: 0; overflow-x: hidden; overflow-y: auto;",
		"max-height: min(42rem, 62vh);",
		"scrollbar-gutter: stable;",
		"overscroll-behavior: contain;",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("repo-card scroll CSS missing %q", want)
		}
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
