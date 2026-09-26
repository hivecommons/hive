package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestRepoCardPRBandsStaticWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"function prBandInfo(pr, held)",
		"function groupedRepoPRs(openPrs, heldPrs)",
		"const prPills = groupedRepoPRs(r.openPrs || [], r.heldPrs || []).map(g => {",
		"repo-pr-band-title",
		"PR_HUMAN_GATE_LABELS",
		"PR_BAND_ORDER",
		"prUpdatedAt",
		"prReviewClassRank",
		"prSignalHTML(bandInfo)",
		"Waiting on human",
		"Merge-eligible",
		"✗ CI",
		"⑂",
		"🕒",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	if strings.Contains(html, "heldPrPills") {
		t.Error("held PRs are still rendered as a trailing append instead of joining PR bands")
	}
}

func TestRepoCardPRBandsBehaviour(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: repository PR banding was not executed")
	}
	html := indexHTML(t)
	funcs := []string{
		"normalizeIssueBandConfig",
		"repoIssueBandConfig",
		"canonicalHiveHoldLabel",
		"holdLabels",
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
	}
	var script strings.Builder
	script.WriteString(`
const assert = require('node:assert/strict');
const REPO_ISSUE_BAND_DEFAULTS = {
  waitingLabels: ['blocked', 'needs-decision', '2-discussing', 'Epic', 'needs-human', 'needs-triage'],
  doneLabels: ['hive/already-done'],
  staleDays: 14
};
const MS_PER_DAY = 24 * 60 * 60 * 1000;
const HOLD_LABEL_SPELLINGS = ['hold', 'on-hold', 'hold/review'];
const PR_HUMAN_GATE_LABELS = ['needs-human', 'needs-decision', '2-discussing'];
const PR_BAND_ORDER = ['waiting', 'eligible', 'blocked', 'in-review', 'open', 'draft'];
const window = { _repoIssueBandConfig: Object.assign({}, REPO_ISSUE_BAND_DEFAULTS), _hiveAutoMergeLabel: 'automerge', _lastStatus: { hiveId: 'h1' } };
Date.now = () => Date.parse('2026-09-25T00:00:00Z');
`)
	for _, name := range funcs {
		script.WriteString(jsFunc(t, html, name))
		script.WriteByte('\n')
	}
	script.WriteString(`
window._repoIssueBandConfig = normalizeIssueBandConfig({ waiting_labels: ['wait-human'], stale_days: 7 });
assert.equal(prBandInfo({ labels: ['needs-human'], merge_verdict: { state: 'eligible' }, updated_at: '2026-09-24T00:00:00Z' }, false).band, 'waiting');
assert.equal(prBandInfo({ labels: [], merge_verdict: { state: 'eligible' }, updated_at: '2026-09-24T00:00:00Z' }, false).band, 'eligible');
assert.equal(prBandInfo({ labels: ['automerge'], merge_verdict: { state: 'unknown' }, updated_at: '2026-09-24T00:00:00Z' }, false).band, 'eligible');
assert.equal(prBandInfo({ labels: [], merge_verdict: { state: 'blocked' }, updated_at: '2026-09-24T00:00:00Z' }, false).band, 'blocked');
assert.equal(prBandInfo({ labels: [], ci_status: 'failing', failing_checks: ['test'], updated_at: '2026-09-24T00:00:00Z' }, false).band, 'blocked');
assert.equal(prBandInfo({ labels: [], mergeable: 'no', updated_at: '2026-09-24T00:00:00Z' }, false).band, 'blocked');
assert.equal(prBandInfo({ labels: [], merge_verdict: { state: 'outstanding' }, updated_at: '2026-09-24T00:00:00Z' }, false).band, 'in-review');
assert.equal(prBandInfo({ labels: [], review_url: 'https://example.test/review', updated_at: '2026-09-24T00:00:00Z' }, false).band, 'in-review');
assert.equal(prBandInfo({ labels: [], draft: true, updated_at: '2026-09-24T00:00:00Z' }, false).band, 'draft');
assert.equal(prBandInfo({ labels: ['wait-human'], merge_verdict: { state: 'eligible' }, updated_at: '2026-09-24T00:00:00Z' }, false).band, 'waiting');
assert.equal(prBandInfo({ labels: [], merge_verdict: { state: 'eligible' }, updated_at: '2026-09-24T00:00:00Z' }, true).band, 'waiting');
const groups = groupedRepoPRs([
  { number: 7, labels: [], draft: true, updated_at: '2026-09-15T00:00:00Z', created_at: '2026-09-01T00:00:00Z' },
  { number: 4, labels: [], review_url: 'u', updated_at: '2026-09-14T00:00:00Z', created_at: '2026-09-01T00:00:00Z' },
  { number: 2, labels: [], merge_verdict: { state: 'eligible' }, updated_at: '2026-09-18T00:00:00Z', created_at: '2026-09-01T00:00:00Z' },
  { number: 6, labels: [], updated_at: '2026-09-13T00:00:00Z', created_at: '2026-09-01T00:00:00Z' },
  { number: 5, labels: [], merge_verdict: { state: 'blocked' }, updated_at: '2026-09-12T00:00:00Z', created_at: '2026-09-01T00:00:00Z' },
  { number: 3, labels: [], merge_verdict: { state: 'eligible' }, updated_at: '2026-09-10T00:00:00Z', created_at: '2026-09-01T00:00:00Z', review_class: 'tests' },
  { number: 8, labels: [], merge_verdict: { state: 'eligible' }, updated_at: '2026-09-10T00:00:00Z', created_at: '2026-09-01T00:00:00Z', review_class: 'fix' }
], [
  { number: 1, labels: [], merge_verdict: { state: 'eligible' }, updated_at: '2026-09-20T00:00:00Z', created_at: '2026-09-01T00:00:00Z' }
]);
assert.deepEqual(groups.map(g => g.band), ['waiting', 'eligible', 'blocked', 'in-review', 'open', 'draft']);
assert.deepEqual(groups[0].prs.map(e => e.pr.number), [1]);
assert.deepEqual(groups[1].prs.map(e => e.pr.number), [8, 3, 2]);
assert.equal(prIsStale({ updated_at: '2026-09-01T00:00:00Z' }), true);
`)
	if out, err := exec.Command(node, "-e", script.String()).CombinedOutput(); err != nil {
		t.Fatalf("repository PR banding check failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
