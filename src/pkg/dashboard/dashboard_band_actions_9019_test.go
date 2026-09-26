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
		"human_acknowledged",
		"const ACTION_BANDS =",
		"function actionBandDef(kind, band)",
		"function actionBandRule(kind, band)",
		"needsTriage: !!role && !acknowledged",
		// Legend band rows are generated from the spec tables, not hand-written.
		"ACTION_BANDS.issue.map(b => {",
		"ACTION_BANDS.pr.map(b => `<span class=\"repo-pr-band-title\" title=\"${esc(actionBandRule('pr', b.key))}\">",
		// Repo-card band headers carry the rule.
		`<div class="repo-issue-band-title" title="${esc(g.rule)}">`,
		`<div class="repo-pr-band-title" title="${esc(g.rule)}">`,
		// Overview legend rows and slices carry the rule.
		`<div class="overview-chart-legend-row" title="${esc(s.rule)}">`,
		"<title>${esc(s.rule)}</title>",
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
		"actionBandDef",
		"actionBandRule",
		"canonicalHiveHoldLabel",
		"holdLabels",
		"issueLabelSet",
		"issueHasAnyLabel",
		"issueAgentRole",
		"issueClaimed",
		"issueLinkedPRState",
		"issueUpdatedAt",
		"issueIsStale",
		"issueBandInfo",
		"issueBandLabel",
		"issueBandShortLabel",
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
		"prBandLabel",
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
const ACTION_BANDS = {
  issue: [
    { key: 'unclaimed', label: 'Unclaimed', shortLabel: 'unclaimed', rule: 'Unclaimed: no higher-priority display state matched; no operator action is required before agents may consider it.' },
    { key: 'in-progress', label: 'Claimed', shortLabel: 'claimed', rule: 'Claimed: an assignee, claim marker, or open linked PR shows someone is already working it.' },
    { key: 'needs-triage', label: 'Needs triage', shortLabel: 'triage', rule: 'Needs triage: an agent filed this issue and no human acknowledgement is present (approved-direction, human assignee, or first-page human comment).' },
    { key: 'waiting', label: 'Needs human', shortLabel: 'needs human', rule: 'Needs human: a waiting label such as blocked, needs-decision, 2-discussing, Epic, needs-human, or needs-triage says agents need human input.' },
    { key: 'done', label: 'Confirm & close', shortLabel: 'close?', rule: 'Confirm & close: an agent applied hive/already-done, hive/covered-by-pr, or hive/likely-done, or a merged linked PR exists; verify and close.' }
  ],
  pr: [
    { key: 'waiting', label: 'Needs human', shortLabel: 'needs human', rule: 'Needs human: this PR is held, has needs-human, or carries a configured waiting label.' },
    { key: 'eligible', label: 'Merge-eligible', shortLabel: 'eligible', rule: 'Merge-eligible: the sweep verdict says eligible or the PR is queued for auto-merge.' },
    { key: 'blocked', label: 'Blocked', shortLabel: 'blocked', rule: 'Blocked: the merge verdict is blocked, GitHub reports conflicts, or CI is failing.' },
    { key: 'in-review', label: 'In review', shortLabel: 'review', rule: 'In review: the PR has an outstanding verdict, a Hive review, or a GitHub review decision.' },
    { key: 'open', label: 'Open', shortLabel: 'open', rule: 'Open: ordinary open PRs with no more specific display state.' },
    { key: 'draft', label: 'Draft', shortLabel: 'draft', rule: 'Draft: GitHub marks this pull request as a draft.' }
  ]
};
const ISSUE_BAND_ORDER = ACTION_BANDS.issue.map(b => b.key);
const OVERVIEW_ISSUE_BAND_ORDER = ISSUE_BAND_ORDER;
const PR_BAND_ORDER = ACTION_BANDS.pr.map(b => b.key);
const OVERVIEW_CHART_FULL = 100, OVERVIEW_CHART_VIEWBOX = 240, OVERVIEW_CHART_CENTER = 120, OVERVIEW_CHART_RADIUS = 90;
const OVERVIEW_CHART_STROKE = 40, OVERVIEW_CHART_PERCENT_SCALE = 100, OVERVIEW_CHART_DEGREES = 360, OVERVIEW_CHART_START_ANGLE = -90;
const OVERVIEW_CHART_LABEL_RADIUS = 90, OVERVIEW_CHART_LABEL_MIN_PERCENT = 8, OVERVIEW_CHART_DECIMAL_PLACES = 1;
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
assert.equal(issueBandInfo(filed).band, 'needs-triage');
assert.equal(issueBandInfo(Object.assign({}, filed, { human_acknowledged: true })).band, 'unclaimed');
assert.equal(issueBandInfo({ labels: ['agent/strategist', 'Approved-Direction'], updated_at: at }).band, 'unclaimed');
assert.equal(issueBandInfo(Object.assign({}, filed, { human_acknowledged: true, assignees: ['dan'] })).band, 'in-progress');
// A held agent issue a human is assigned to, exactly as HoldItem serializes it.
const heldAcked = { number: 7, repo: 'o/r', title: 'held', type: 'issue', labels: ['hold', 'agent/strategist'], assignees: ['dan'], human_acknowledged: true, created_at: at };
assert.equal(issueBandInfo(heldAcked).band, 'in-progress');
assert.equal(issueBandInfo(Object.assign({}, heldAcked, { assignees: undefined })).band, 'unclaimed');
assert.equal(issueBandInfo({ number: 8, type: 'issue', labels: ['hold', 'agent/strategist'], created_at: at }).band, 'needs-triage');
// Precedence above the triage band is unchanged.
assert.equal(issueBandInfo({ labels: ['agent/strategist', 'blocked'], updated_at: at }).band, 'waiting');
assert.equal(issueBandInfo({ labels: ['agent/strategist', 'hive/likely-done'], updated_at: at }).band, 'done');
// The role stays on the pill either way, and the tooltip says which it is.
const ackSignals = issueBandInfo(Object.assign({}, filed, { human_acknowledged: true })).signals;
assert.equal(ackSignals.find(s => s.role).label, 'filed by agent role strategist; human acknowledged');
assert.equal(issueBandInfo(filed).signals.find(s => s.role).label, 'filed by agent role strategist');
// One triage band, not one per role.
const groups = groupedRepoIssues([
  { number: 1, labels: ['agent/strategist'], updated_at: at },
  { number: 2, labels: ['agent/quality'], updated_at: at },
  { number: 3, labels: ['agent/quality'], human_acknowledged: true, updated_at: at }
]);
assert.deepEqual(groups.map(g => [g.band, g.label, g.issues.length]), [['unclaimed', 'Unclaimed', 1], ['needs-triage', 'Needs triage', 2]]);

// 3. Rule text comes from ACTION_BANDS and is the same everywhere.
window._repoIssueBandConfig = normalizeIssueBandConfig({ waiting_labels: ['wait-human'], done_labels: ['done-custom'] });
assert.equal(actionBandRule('issue', 'done'), ACTION_BANDS.issue.find(b => b.key === 'done').rule);
assert.equal(actionBandRule('pr', 'blocked'), ACTION_BANDS.pr.find(b => b.key === 'blocked').rule);
assert.equal(groupedRepoIssues([{ number: 9, labels: ['done-custom'], updated_at: at }])[0].rule, actionBandRule('issue', 'done'));
assert.equal(groupedRepoPRs([{ number: 9, labels: [], draft: true, updated_at: at, created_at: at }], [])[0].rule, actionBandRule('pr', 'draft'));

const repos = [{ actionableIssues: [{ number: 9, labels: ['done-custom'], updated_at: at }], heldIssues: [], openPrs: [], heldPrs: [] }];
const slices = overviewIssueBandSlices(repos);
assert.deepEqual(slices.map(s => s.rule), OVERVIEW_ISSUE_BAND_ORDER.map(band => actionBandRule('issue', band)));
assert.deepEqual(overviewPRBandSlices(repos).map(s => s.rule), PR_BAND_ORDER.map(band => actionBandRule('pr', band)));
const donut = renderOverviewDonut('Issues by band', 'sub', slices);
assert.ok(donut.includes('<title>' + esc(actionBandRule('issue', 'done')) + '</title>'), donut);
assert.ok(donut.includes('<div class="overview-chart-legend-row" title="' + esc(actionBandRule('issue', 'done')) + '">'), donut);
`)
	if out, err := exec.Command(node, "-e", script.String()).CombinedOutput(); err != nil {
		t.Fatalf("band action naming check failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
