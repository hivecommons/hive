package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestOverviewCSVExportStaticWiring9056(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`class="hv-btn btn-sm overview-export-csv" data-action="downloadOverviewCsv" data-arg0="${csvKind}"`,
		`const csvDisabled = total ? '' : ' disabled';`,
		`title="${esc(csvTitle)}"${csvDisabled}>Export CSV</button>`,
		`class="overview-band-download" data-action="downloadOverviewCsv" data-arg0="${esc(kind)}" data-arg1="${esc(s.key)}"`,
		`const visible = (slices || []).filter(s => s.count > 0);`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	if strings.Contains(html, "onclick=\"downloadOverviewCsv") || strings.Contains(html, "onclick='downloadOverviewCsv") {
		t.Errorf("CSV export must use delegated data-action wiring, not inline onclick")
	}
}

func TestOverviewCSVExportBehaviour9056(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: Overview CSV export behaviour was not executed")
	}
	html := indexHTML(t)
	funcs := []string{
		"esc",
		"normalizeIssueBandConfig",
		"repoIssueBandConfig",
		"canonicalHiveHoldLabel",
		"holdLabels",
		"heldReason",
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
		"issueBandRule",
		"issueBandTip",
		"issueBandRank",
		"groupedRepoIssues",
		"overviewRepoName",
		"overviewIssueBandSlices",
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
		"overviewPRBandSlices",
		"overviewCsvCell",
		"overviewCsv",
		"overviewCsvColumns",
		"overviewBandSlug",
		"overviewItemUrl",
		"overviewSignalLabels",
		"overviewLinkedPRs",
		"overviewMergeVerdictText",
		"overviewReviewDecision",
		"overviewIssueCsvRow",
		"overviewPRCsvRow",
		"overviewCsvRows",
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
const HOLD_LABEL_SPELLINGS = ['hold', 'on-hold', 'hold/review'];
const window = { _repoIssueBandConfig: Object.assign({}, REPO_ISSUE_BAND_DEFAULTS), _hiveAutoMergeLabel: 'automerge', _lastStatus: { hiveId: 'h1' } };
let _overviewLastRepos = [];
Date.now = () => Date.parse('2026-09-25T00:00:00Z');
`)
	for _, name := range funcs {
		script.WriteString(jsFunc(t, html, name))
		script.WriteByte('\n')
	}
	script.WriteString(`
const repos = [{
  full: 'octo/demo',
  actionableIssues: [
    { number: 5, title: 'Needs "quotes", comma 😀', labels: [], assignees: [], updated_at: '2026-09-01T00:00:00Z', url: 'https://github.com/octo/demo/issues/5' },
    { number: 2, title: 'assigned', labels: [], assignees: ['dan'], updated_at: '2026-09-03T00:00:00Z' },
    { number: 4, title: 'agent filed', labels: ['agent/strategist'], updated_at: '2026-09-02T00:00:00Z' },
    { number: 6, title: 'waiting', labels: ['needs-human'], updated_at: '2026-09-04T00:00:00Z' },
    { number: 7, title: 'done', labels: ['hive/likely-done'], updated_at: '2026-09-05T00:00:00Z' }
  ],
  heldIssues: [
    { number: 3, title: 'held triage', labels: ['agent/quality', 'hold'], updated_at: '2026-09-01T12:00:00Z' }
  ],
  openPrs: [
    { number: 10, title: 'needs human', labels: ['needs-human'], updated_at: '2026-09-01T00:00:00Z', created_at: '2026-08-31T00:00:00Z', author: 'alice' },
    { number: 11, title: 'eligible', labels: ['automerge'], merge_verdict: { state: 'eligible' }, updated_at: '2026-09-02T00:00:00Z', created_at: '2026-09-01T00:00:00Z', failing_checks: [] },
    { number: 12, title: 'blocked', labels: [], mergeable: 'no', ci_status: 'failing', updated_at: '2026-09-03T00:00:00Z', created_at: '2026-09-02T00:00:00Z', failing_checks: ['ci/test'] },
    { number: 13, title: 'review', labels: [], review_url: 'https://reviews/13', updated_at: '2026-09-04T00:00:00Z', created_at: '2026-09-03T00:00:00Z' },
    { number: 14, title: 'open', labels: [], updated_at: '2026-09-05T00:00:00Z', created_at: '2026-09-04T00:00:00Z' },
    { number: 15, title: 'draft', labels: [], draft: true, updated_at: '2026-09-06T00:00:00Z', created_at: '2026-09-05T00:00:00Z' }
  ],
  heldPrs: [
    { number: 9, title: 'held pr', labels: ['hold'], updated_at: '2026-08-30T00:00:00Z', created_at: '2026-08-29T00:00:00Z' }
  ]
}];
_overviewLastRepos = repos;
const issueSlices = overviewIssueBandSlices(repos);
const prSlices = overviewPRBandSlices(repos);
assert.deepEqual(issueSlices.map(s => s.key), OVERVIEW_ISSUE_BAND_ORDER);
assert.deepEqual(prSlices.map(s => s.key), PR_BAND_ORDER);
assert.deepEqual(issueSlices.map(s => s.count), [1, 1, 2, 1, 1]);
assert.deepEqual(prSlices.map(s => s.count), [2, 1, 1, 1, 1, 1]);
assert.deepEqual(issueSlices.map(s => s.items.length), issueSlices.map(s => s.count));
assert.deepEqual(prSlices.map(s => s.items.length), prSlices.map(s => s.count));
assert.deepEqual(issueSlices.find(s => s.key === 'agent-filed').items.map(i => i.number), [3, 4]);
assert.deepEqual(prSlices.find(s => s.key === 'waiting').items.map(p => p.number), [9, 10]);

const issueRows = overviewCsvRows('issues');
const prRows = overviewCsvRows('prs');
assert.equal(issueRows.length, issueSlices.reduce((n, s) => n + s.count, 0));
assert.equal(prRows.length, prSlices.reduce((n, s) => n + s.count, 0));
assert.deepEqual(issueRows.map(r => r.band), ['Unclaimed', 'Claimed', 'Needs triage', 'Needs triage', 'Needs human', 'Confirm & close']);
assert.deepEqual(prRows.map(r => r.band), ['Needs human', 'Needs human', 'Merge-eligible', 'Blocked', 'In review', 'Open', 'Draft']);
assert.equal(issueRows.find(r => r.number === 3).held, true);
assert.equal(issueRows.find(r => r.number === 4).agent_role, 'strategist');
assert.equal(issueRows.find(r => r.number === 4).band_rule, issueBandRule('agent-filed'));
assert.equal(prRows.find(r => r.number === 12).signals.includes('failing CI: ci/test'), true);
assert.equal(prRows.find(r => r.number === 12).band_rule, prBandRule('blocked'));

const csv = overviewCsv(issueRows, overviewCsvColumns('issues'));
assert.equal(csv.charCodeAt(0), 0xfeff);
assert.equal(csv.includes('\r\n'), true);
assert.equal(csv.includes('\n') && !csv.includes('\r\n'), false);
assert.equal(csv.includes('"Needs ""quotes"", comma 😀"'), true);
const triageRows = overviewCsvRows('issues', 'agent-filed');
assert.deepEqual(triageRows, issueRows.filter(r => r.band === issueBandLabel('agent-filed')));
assert.deepEqual(overviewCsvRows('prs', 'waiting'), prRows.filter(r => r.band === prBandLabel('waiting')));
assert.equal(overviewBandSlug('Needs triage'), 'needs-triage');
`)
	if out, err := exec.Command(node, "-e", script.String()).CombinedOutput(); err != nil {
		t.Fatalf("overview CSV export check failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
