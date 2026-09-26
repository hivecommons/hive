package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// #9019: bands are named after the operator's action, the agent-filed band
// empties once a human acknowledges the proposal, and every place a band is
// named (repo-card header, Overview slice and legend row, pill legend) carries
// the same rule text from one spec table.
func TestDashboardBandActionsStaticWiring9019(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"function issueAcknowledged(issue)",
		"function issueBandSpec(band)",
		"function prBandSpec(band)",
		"agentFiled: !!role && !acknowledged",
		// Legend band rows are generated from the spec tables, not hand-written.
		"const issueBands = OVERVIEW_ISSUE_BAND_ORDER.map(band => {",
		"const prBands = PR_BAND_ORDER.map(band => `<span class=\"repo-pr-band-title\" title=\"${esc(prBandTip(band))}\">",
		// Repo-card band headers carry the rule.
		`<div class="repo-issue-band-title" title="${esc(g.tip)}">`,
		`<div class="repo-pr-band-title" title="${esc(g.tip)}">`,
		// Overview legend rows and slices carry the rule.
		`<div class="overview-chart-legend-row" title="${esc(s.label + (s.rule ? ': ' + s.rule : ''))}">`,
		"<title>${esc(s.label)}: ${s.count}${s.rule ? ' — ' + esc(s.rule) : ''}</title>",
		"hover a band for its rule",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"'Likely done'",
		"'Agent-filed'",
		"'Waiting on human'",
		"'Ready'",
		"'In progress'",
		"agent-filed</span>",
		"likely done</span>",
		"Band: needs-human, held, or configured waiting labels",
	} {
		if strings.Contains(html, forbidden) {
			t.Errorf("index.html still names a classifier band %q", forbidden)
		}
	}
}

func TestDashboardBandActionsBehaviour9019(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: band action naming was not executed")
	}
	html := indexHTML(t)
	funcs := []string{
		"esc",
		"normalizeIssueBandConfig",
		"repoIssueBandConfig",
		"canonicalHiveHoldLabel",
		"holdLabels",
		"issueLabelSet",
		"issueHasAnyLabel",
		"issueAgentRole",
		"issueClaimed",
		"issueAcknowledged",
		"issueLinkedPRState",
		"issueUpdatedAt",
		"issueIsStale",
		"issueBandInfo",
		"issueBandSpec",
		"issueBandLabel",
		"issueBandShortLabel",
		"issueBandRule",
		"issueBandTip",
		"issueBandRank",
		"groupedRepoIssues",
		"prLabelSet",
		"prHasAnyLabel",
		"prQueued",
		"prAgentRole",
		"prUpdatedAt",
		"prCreatedAt",
		"prReviewClassRank",
		"prIsStale",
		"prCIFailing",
		"prGitHubReview",
		"prRequestedReviews",
		"prConversation",
		"prBandInfo",
		"prBandSpec",
		"prBandLabel",
		"prBandRule",
		"prBandTip",
		"prBandRank",
		"groupedRepoPRs",
		"overviewIssueBandSlices",
		"overviewPRBandSlices",
		"overviewChartLabelPoint",
		"renderOverviewDonut",
	}
	var script strings.Builder
	script.WriteString(`
const assert = require('node:assert/strict');
const REPO_ISSUE_BAND_DEFAULTS = {
  waitingLabels: ['blocked', 'needs-decision', '2-discussing', 'Epic', 'needs-human', 'needs-triage'],
  doneLabels: ['hive/already-done', 'hive/covered-by-pr', 'hive/likely-done'],
  staleDays: 14
};
const MS_PER_DAY = 24 * 60 * 60 * 1000;
const PR_HUMAN_GATE_LABELS = ['needs-human', 'needs-decision', '2-discussing'];
const PR_BAND_ORDER = ['waiting', 'eligible', 'blocked', 'in-review', 'open', 'draft'];
const OVERVIEW_ISSUE_BAND_ORDER = ['ready', 'in-progress', 'agent-filed', 'waiting', 'done'];
const OVERVIEW_CHART_FULL = 100, OVERVIEW_CHART_VIEWBOX = 240, OVERVIEW_CHART_CENTER = 120, OVERVIEW_CHART_RADIUS = 90;
const OVERVIEW_CHART_STROKE = 40, OVERVIEW_CHART_PERCENT_SCALE = 100, OVERVIEW_CHART_DEGREES = 360, OVERVIEW_CHART_START_ANGLE = -90;
const OVERVIEW_CHART_LABEL_RADIUS = 90, OVERVIEW_CHART_LABEL_MIN_PERCENT = 8, OVERVIEW_CHART_DECIMAL_PLACES = 1;
const OVERVIEW_CHART_LABEL_POLE_DEGREES = 12;
const OVERVIEW_CHART_TOTAL_VALUE_Y_OFFSET = 4, OVERVIEW_CHART_TOTAL_CAPTION_Y_OFFSET = 22;
const window = { _repoIssueBandConfig: Object.assign({}, REPO_ISSUE_BAND_DEFAULTS), _hiveAutoMergeLabel: 'automerge', _lastStatus: { hiveId: 'h1' } };
Date.now = () => Date.parse('2026-09-25T00:00:00Z');
`)
	for _, name := range funcs {
		script.WriteString(jsFunc(t, html, name))
		script.WriteByte('\n')
	}
	script.WriteString(`
const at = '2026-09-24T00:00:00Z';

// 1. Band names are operator actions (or an explicit "nothing needed").
assert.deepEqual(OVERVIEW_ISSUE_BAND_ORDER.map(issueBandLabel), ['Unclaimed', 'Claimed', 'Needs triage', 'Needs human', 'Confirm & close']);
assert.deepEqual(OVERVIEW_ISSUE_BAND_ORDER.map(issueBandShortLabel), ['unclaimed', 'claimed', 'triage', 'needs human', 'close?']);
assert.deepEqual(PR_BAND_ORDER.map(prBandLabel), ['Needs human', 'Merge-eligible', 'Blocked', 'In review', 'Open', 'Draft']);
assert.equal(issueBandLabel('bogus'), 'Unclaimed');
assert.equal(prBandLabel('bogus'), 'Open');

// 2. Agent-filed is keyed on acknowledgment: the server flag or the
// approved-direction label empties the band, for actionable and held issues
// alike (HoldItem carries assignees + human_acknowledged too).
const filed = { labels: ['agent/strategist'], updated_at: at };
assert.equal(issueBandInfo(filed).band, 'agent-filed');
assert.equal(issueBandInfo(Object.assign({}, filed, { human_acknowledged: true })).band, 'ready');
assert.equal(issueBandInfo({ labels: ['agent/strategist', 'Approved-Direction'], updated_at: at }).band, 'ready');
assert.equal(issueBandInfo(Object.assign({}, filed, { human_acknowledged: true, assignees: ['dan'] })).band, 'in-progress');
// A held agent issue a human is assigned to, exactly as HoldItem serializes it.
const heldAcked = { number: 7, repo: 'o/r', title: 'held', type: 'issue', labels: ['hold', 'agent/strategist'], assignees: ['dan'], human_acknowledged: true, created_at: at };
assert.equal(issueBandInfo(heldAcked).band, 'in-progress');
assert.equal(issueBandInfo(Object.assign({}, heldAcked, { assignees: undefined })).band, 'ready');
assert.equal(issueBandInfo({ number: 8, type: 'issue', labels: ['hold', 'agent/strategist'], created_at: at }).band, 'agent-filed');
// Precedence above the triage band is unchanged.
assert.equal(issueBandInfo({ labels: ['agent/strategist', 'blocked'], updated_at: at }).band, 'waiting');
assert.equal(issueBandInfo({ labels: ['agent/strategist', 'hive/likely-done'], updated_at: at }).band, 'done');
// The role stays on the pill either way, and the tooltip says which it is.
const ackSignals = issueBandInfo(Object.assign({}, filed, { human_acknowledged: true })).signals;
assert.equal(ackSignals.find(s => s.role).label, 'agent-filed by strategist, acknowledged by a human');
assert.equal(issueBandInfo(filed).signals.find(s => s.role).label, 'agent-filed by strategist, not yet acknowledged');
// One triage band, not one per role.
const groups = groupedRepoIssues([
  { number: 1, labels: ['agent/strategist'], updated_at: at },
  { number: 2, labels: ['agent/quality'], updated_at: at },
  { number: 3, labels: ['agent/quality'], human_acknowledged: true, updated_at: at }
]);
assert.deepEqual(groups.map(g => [g.band, g.label, g.issues.length]), [['ready', 'Unclaimed', 1], ['agent-filed', 'Needs triage', 2]]);

// 3. Rule text follows the configured taxonomy and is the same everywhere.
window._repoIssueBandConfig = normalizeIssueBandConfig({ waiting_labels: ['wait-human'], done_labels: ['done-custom'] });
assert.equal(issueBandRule('done'), 'an agent applied done-custom or a merged PR references it — verify the work landed and close the issue');
assert.equal(issueBandRule('waiting'), 'labelled wait-human — a human must unblock or decide before agents continue');
assert.equal(prBandRule('waiting'), 'held, or labelled needs-human, needs-decision, 2-discussing, wait-human — a human must review, decide, or release the hold before automation continues');
assert.equal(issueBandTip('done'), 'Confirm & close: ' + issueBandRule('done'));
assert.equal(prBandTip('blocked'), 'Blocked: ' + prBandRule('blocked'));
assert.equal(groupedRepoIssues([{ number: 9, labels: ['done-custom'], updated_at: at }])[0].tip, issueBandTip('done'));
assert.equal(groupedRepoPRs([{ number: 9, labels: [], draft: true, updated_at: at, created_at: at }], [])[0].tip, prBandTip('draft'));

const repos = [{ actionableIssues: [{ number: 9, labels: ['done-custom'], updated_at: at }], heldIssues: [], openPrs: [], heldPrs: [] }];
const slices = overviewIssueBandSlices(repos);
assert.deepEqual(slices.map(s => s.rule), OVERVIEW_ISSUE_BAND_ORDER.map(issueBandRule));
assert.deepEqual(overviewPRBandSlices(repos).map(s => s.rule), PR_BAND_ORDER.map(prBandRule));
const donut = renderOverviewDonut('Issues by band', 'sub', slices);
assert.ok(donut.includes('<title>Confirm &amp; close: 1 — ' + esc(issueBandRule('done')) + '</title>'), donut);
assert.ok(donut.includes('<div class="overview-chart-legend-row" title="' + esc(issueBandTip('done')) + '">'), donut);
`)
	if out, err := exec.Command(node, "-e", script.String()).CombinedOutput(); err != nil {
		t.Fatalf("band action naming check failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
