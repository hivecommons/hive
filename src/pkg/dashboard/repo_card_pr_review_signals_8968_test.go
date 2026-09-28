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
	funcs := []string{"prGitHubReview", "prRequestedReviews", "prConversation", "prLinkedIssues"}
	var script strings.Builder
	script.WriteString("const assert = require('node:assert/strict');\n")
	for _, name := range funcs {
		script.WriteString(jsFunc(t, html, name))
		script.WriteByte('\n')
	}
	script.WriteString(`
const fresh = { labels: [], updated_at: '2026-09-24T00:00:00Z' };

// No protection facts at all: nothing is invented, and the PR stays Open.
assert.equal(prGitHubReview(fresh), null);

assert.equal(prGitHubReview({protection: {review_decision:'APPROVED', approvals_given:2}}).label, 'approved on GitHub (2 approvals)');
assert.equal(prGitHubReview({protection: {review_decision:'CHANGES_REQUESTED', changes_requested_by:['octo','cat']}}).label, 'changes requested by @octo, @cat');
assert.equal(prGitHubReview({protection: {review_decision:'REVIEW_REQUIRED', approvals_given:1}}).label, 'approving review required by branch protection (1 approval given)');

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
assert.equal(prGitHubReview({ protection: { required_checks_known: true } }), null);
assert.equal(prGitHubReview({ protection: { approvals_given: 0, changes_requested_by: [] } }), null);

// Requested reviewers and teams are named, in that order.
assert.deepEqual(prRequestedReviews({ requested_reviewers: ['alice'], requested_teams: ['maintainers'] }), ['@alice', 'team maintainers']);
assert.deepEqual(prRequestedReviews({}), []);

// Conversation volume: a single glyph carrying the total, the tooltip
// splitting comments from review threads; silent when both are zero.
assert.equal(prConversation(fresh), null);
assert.equal(prConversation({ comment_count: 0, review_thread_count: 0 }), null);
assert.deepEqual(prConversation({ comment_count: 1 }), { total: 1, label: '1 comment' });
assert.deepEqual(prConversation({ comment_count: 3, review_thread_count: 2 }), { total: 5, label: '3 comments, 2 review threads' });

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
		`glyph: '👍', caption: 'approved on GitHub', tip: 'Approved on GitHub; tooltip counts the approvals'`,
		`glyph: '👎', caption: 'changes requested', tip: 'Changes requested on GitHub; tooltip names the reviewers'`,
		`glyph: '👀', caption: 'approval required, not yet given', tip: 'Approving review required by branch protection and not yet given'`,
		`glyph: '👥', caption: 'review requested from someone', tip: 'Review requested; tooltip lists the reviewers and teams'`,
		`label: '🗨 3', caption: 'comment / thread count', tip: 'Comments and review threads on GitHub'`,
		`label: '🔗 #42', caption: 'on a PR: issue it closes on merge', tip: 'Issue this PR closes on merge (GitHub closing reference); opens the issue'`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}
