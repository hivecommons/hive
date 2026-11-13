package github

import (
	"context"
	"fmt"
)

// Three more automated closes leave an issue with an open sub-issue alone.
//
// An issue whose remaining work was moved into sub-issues is not done while
// one of them is open. The contribute hub's verdict settle, close-on-merge
// and the issue stage sweep already hold such an issue back. Three other
// paths closed it as completed without looking:
//
//   - the already-landed close (closeIssuesAlreadyLanded): the issues a PR
//     closed as "already landed" names with a closing keyword;
//   - the question auto-close (pkg/questionclose): an answered question at
//     the end of its objection window;
//   - an agent's "close" request (the issue-request watcher).
//
// All three now read GitHub's own `sub_issues_summary` block off the issue
// payload. The first two take it from the issue read they already made
// (getIssueWithBlockedCount in place of Issues.Get, the same request with
// the summary kept), so they make no new request and gain no new way to
// fail. The agent's close request had no issue read of its own (the close
// helper reads the issue itself), so it makes one, before anything is
// written; a failure of that read is handled exactly like a failure of the
// close helper's own read of the same issue.
//
// Only native GitHub sub-issues are counted. The summary counts a sub-issue
// closed for any reason as completed, so "open" here means still open. An
// issue with no sub-issues, with every sub-issue closed, or on a host whose
// payload carries no summary is handled exactly as before, and so is a pull
// request. The split-parents ledger is not consulted: its only writer
// records a child after the native sub-issue link succeeded.
//
// None of the three paths comes back to the issue on its own. With stage
// labels on (governor.labels.stage.enabled, the default) the issue stage
// sweep closes the parent once every sub-issue is closed
// (closeDecomposedParent); with them off that sweep does not run, and the
// issue stays open until a person closes it. The one exception is the
// question auto-close, which judges the same answer again after its settled
// record is pruned (see pkg/questionclose).

// automatedCloseOpenSubIssues is how many of an issue's sub-issues GitHub's
// summary says are still open. A nil summary (the issue has no sub-issues,
// or the host does not report them) is zero.
func automatedCloseOpenSubIssues(summary *IssueChildren) int {
	if summary == nil {
		return 0
	}
	return summary.Open()
}

// automatedCloseSubIssuesRefusal is the sentence an agent reads when its
// close request is refused. It says when the issue can be closed and who
// can close it sooner, and does not tell the agent to close the sub-issues:
// those are open work, not an obstacle to clear.
func automatedCloseSubIssuesRefusal(ref string, open int) string {
	if open == 1 {
		return "issue close refused: " + ref + " has 1 open sub-issue, so it is not finished; it can be closed once that sub-issue is closed, or by a maintainer before then"
	}
	return fmt.Sprintf("issue close refused: %s has %d open sub-issues, so it is not finished; it can be closed once they are all closed, or by a maintainer before then", ref, open)
}

// issueCloseRequestOpenSubIssuesRefusal decides whether an agent's "close"
// request must be refused because the target is an issue with an open
// sub-issue. It returns the reason to give the agent, or "" when the close
// may go ahead: the target is a pull request, has no sub-issues, or has
// them all closed.
//
// A failed read returns the error and no verdict, so the watcher retries
// the request (or quarantines it on a definite refusal such as a 404) the
// way it does when the close helper's own read of the issue fails; the
// wording is that helper's, so the agent reads the same error either way.
// The issue is never closed on an unknown answer.
//
// An override reason does not lift the refusal. It exists to close a
// human-filed bug before its reporter confirms (a duplicate, not a bug),
// and says nothing about work still open under the issue.
func (c *Client) issueCloseRequestOpenSubIssuesRefusal(ctx context.Context, repo string, number int) (string, error) {
	if c == nil || c.client == nil {
		// The close helper reports the missing client itself.
		return "", nil
	}
	owner, repoName := c.splitRepo(repo)
	issue, _, summary, err := c.getIssueWithBlockedCount(ctx, owner, repoName, number)
	if err != nil {
		return "", fmt.Errorf("reading issue %s/%s#%d before close: %w", owner, repoName, number, err)
	}
	if issue == nil || issue.IsPullRequest() {
		return "", nil
	}
	open := automatedCloseOpenSubIssues(summary)
	if open == 0 {
		return "", nil
	}
	return automatedCloseSubIssuesRefusal(fmt.Sprintf("%s/%s#%d", owner, repoName, number), open), nil
}
