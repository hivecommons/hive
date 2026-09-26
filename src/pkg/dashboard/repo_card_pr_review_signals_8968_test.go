package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// The repo-card PR pill reads GitHub's review decision, requested reviewers,
// conversation volume, and closing-issue references from the snapshot
// (hivecommons/hive#8968). Everything here is display-only: the band rules
// for human gates, merge eligibility, and blocked verdicts are unchanged, and
// a GitHub review decision only decides membership of the In review band.
func TestRepoCardPRReviewSignalsBehaviour(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: repository PR review signals were not executed")
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
		"prLinkedIssues",
		"prBandInfo",
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
const fresh = { labels: [], updated_at: '2026-09-24T00:00:00Z' };
const glyphs = info => info.signals.map(s => s.glyph);
const labels = info => info.signals.map(s => s.label);

// No protection facts at all: nothing is invented, and the PR stays Open.
assert.equal(prGitHubReview(fresh), null);
assert.equal(prBandInfo(fresh, false).band, 'open');

// GitHub's own decision is the primary source.
const approved = prBandInfo(Object.assign({}, fresh, { protection: { review_decision: 'APPROVED', approvals_given: 2 } }), false);
assert.equal(approved.band, 'in-review');
assert.equal(approved.review.decision, 'approved');
assert.ok(glyphs(approved).includes('👍'));
assert.ok(labels(approved).includes('approved on GitHub (2 approvals)'));

const changes = prBandInfo(Object.assign({}, fresh, { protection: { review_decision: 'CHANGES_REQUESTED', changes_requested_by: ['octo', 'cat'] } }), false);
assert.equal(changes.band, 'in-review');
assert.equal(changes.review.decision, 'changes-requested');
assert.ok(glyphs(changes).includes('👎'));
assert.ok(labels(changes).includes('changes requested by @octo, @cat'));

const required = prBandInfo(Object.assign({}, fresh, { protection: { review_decision: 'REVIEW_REQUIRED', approvals_given: 1 } }), false);
assert.equal(required.band, 'in-review');
assert.equal(required.review.decision, 'review-required');
assert.ok(glyphs(required).includes('👀'));
assert.ok(labels(required).includes('approving review required by branch protection (1 approval given)'));

// A base branch with no review rule reports a null decision. The reviewer
// opinions are then shown as opinions — the decision stays unknown, never
// inferred (ReviewDecisionNone must never read as approved).
const opinionApproved = prGitHubReview({ protection: { approvals_given: 1 } });
assert.equal(opinionApproved.decision, '');
assert.equal(opinionApproved.glyph, '👍');
assert.equal(opinionApproved.label, '1 approval on GitHub — no review decision');
const opinionChanges = prGitHubReview({ protection: { approvals_given: 3, changes_requested_by: ['octo'] } });
assert.equal(opinionChanges.decision, '');
assert.equal(opinionChanges.glyph, '👎');
assert.equal(opinionChanges.label, 'changes requested by @octo — no review decision from GitHub');
assert.equal(prBandInfo(Object.assign({}, fresh, { protection: { approvals_given: 1 } }), false).band, 'in-review');
assert.equal(prGitHubReview({ protection: { required_checks_known: true } }), null);
assert.equal(prGitHubReview({ protection: { approvals_given: 0, changes_requested_by: [] } }), null);

// Precedence is untouched: a human gate, an eligible verdict, and a blocked
// verdict all still win over a review decision.
assert.equal(prBandInfo(Object.assign({}, fresh, { labels: ['needs-human'], protection: { review_decision: 'APPROVED' } }), false).band, 'waiting');
assert.equal(prBandInfo(Object.assign({}, fresh, { merge_verdict: { state: 'eligible' }, protection: { review_decision: 'CHANGES_REQUESTED' } }), false).band, 'eligible');
assert.equal(prBandInfo(Object.assign({}, fresh, { merge_verdict: { state: 'blocked' }, protection: { review_decision: 'APPROVED' } }), false).band, 'blocked');
assert.equal(prBandInfo(Object.assign({}, fresh, { draft: true, protection: { review_decision: 'REVIEW_REQUIRED' } }), false).band, 'in-review');

// Requested reviewers and teams are named, in that order.
assert.deepEqual(prRequestedReviews({ requested_reviewers: ['alice'], requested_teams: ['maintainers'] }), ['@alice', 'team maintainers']);
assert.deepEqual(prRequestedReviews({}), []);
const requestedInfo = prBandInfo(Object.assign({}, fresh, { requested_reviewers: ['alice'], requested_teams: ['maintainers'] }), false);
assert.equal(requestedInfo.band, 'open');
assert.ok(glyphs(requestedInfo).includes('👥'));
assert.ok(labels(requestedInfo).includes('review requested from @alice, team maintainers'));

// Conversation volume: a single glyph carrying the total, the tooltip
// splitting comments from review threads; silent when both are zero.
assert.equal(prConversation(fresh), null);
assert.equal(prConversation({ comment_count: 0, review_thread_count: 0 }), null);
assert.deepEqual(prConversation({ comment_count: 1 }), { total: 1, label: '1 comment' });
assert.deepEqual(prConversation({ comment_count: 3, review_thread_count: 2 }), { total: 5, label: '3 comments, 2 review threads' });
assert.ok(glyphs(prBandInfo(Object.assign({}, fresh, { comment_count: 3, review_thread_count: 2 }), false)).includes('🗨 5'));
assert.ok(!glyphs(prBandInfo(fresh, false)).some(g => String(g).startsWith('🗨')));

// Linked issues come only from the snapshot and drop malformed entries.
assert.deepEqual(prLinkedIssues(fresh), []);
assert.deepEqual(prLinkedIssues({ linked_issues: [{ number: 12, state: 'open' }, null, { state: 'closed' }] }), [{ number: 12, state: 'open' }]);
`)
	if out, err := exec.Command(node, "-e", script.String()).CombinedOutput(); err != nil {
		t.Fatalf("repository PR review signals check failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}

func TestRepoCardPRReviewSignalsStaticWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"function prGitHubReview(pr)",
		"function prRequestedReviews(pr)",
		"function prConversation(pr)",
		"function prLinkedIssues(pr)",
		"repoPillActionCluster([reviewPill, queueBtn + stateBadge, issueBadge, holdBtn])",
		`title="Approved on GitHub; tooltip counts the approvals">👍`,
		`title="Changes requested on GitHub; tooltip names the reviewers">👎`,
		`title="Approving review required by branch protection and not yet given">👀`,
		`title="Review requested; tooltip lists the reviewers and teams">👥`,
		`title="Comments and review threads on GitHub">🗨 3`,
		`title="Issue this PR closes on merge (GitHub closing reference); opens the issue">🔗 #42`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}
