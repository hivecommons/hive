package github

import (
	"context"
	"fmt"
)

// An issue with an open sub-issue is not done.
//
// decomposed_parents.go already resolves a parent's children for the issue
// stage sweep (never stalled while a child is open, closed when all of them
// close). This file is the same relation read by the paths that write "done"
// on an issue from somebody's word that a merged PR settled it: the PR-claim
// label sync (hive/likely-done) and the contribute hub's verdict settle
// (hive/likely-done, the already-done label, the close). A design issue whose
// design PR merged, with the implementation moved to three open sub-issues,
// was offered to a contributor, came back "already done", and was labelled
// done and sent to be closed while every child was still open.

// issueHasOpenChildren reports whether the enumeration row says the issue has
// at least one open tracked child (Issue.Children, set by
// annotateIssueChildren).
func issueHasOpenChildren(issue *Issue) bool {
	return issue != nil && issue.Children != nil && issue.Children.Open() > 0
}

// LiveIssueChildren re-reads one issue's tracked children from GitHub, for a
// caller about to write something that is hard to undo (a close) on evidence
// an enumeration row can lag. The native summary GitHub puts on the issue
// payload answers first, in one GET that every GitHub host serves; when the
// issue has none, the children the split-parents ledger records for it are
// read by number, exactly as closeDecomposedParent does. The zero value
// means the issue has no tracked children. A child known only by its
// `<!-- hive:parent N -->` body marker is not found here: that relation is
// resolved against the open-issue snapshot, so only the enumeration row
// carries it.
func (c *Client) LiveIssueChildren(ctx context.Context, repo string, number int) (IssueChildren, error) {
	if c == nil || c.client == nil {
		return IssueChildren{}, ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	_, _, summary, err := c.getIssueWithBlockedCount(ctx, owner, repoName, number)
	if err != nil {
		return IssueChildren{}, fmt.Errorf("reading sub-issue summary of %s/%s#%d: %w", owner, repoName, number, err)
	}
	if summary != nil && summary.Total > 0 {
		return *summary, nil
	}
	children, open, ok, err := c.splitLedgerChildren(ctx, owner, repoName, number)
	if err != nil || !ok {
		return IssueChildren{}, err
	}
	return IssueChildren{Total: len(children), Completed: len(children) - open}, nil
}

// withIssueChildren returns the patch with a parent's children carried onto
// its actionable issue row. The single-item re-read behind a webhook patch
// builds that row from the issue payload alone, so without this the row that
// replaces the enumerated one has no Children until the next enumeration and
// every reader of the relation (the contributor offer rule, the claim label
// sync) sees a parent as an ordinary issue in between. fresh is the summary
// that same re-read returned; when it names no children, previous (the row
// being replaced) is kept, because a ledger- or marker-resolved relation is
// not on the issue payload. The row is copied, so the caller's patch is
// never mutated.
func (p WebhookItemPatch) withIssueChildren(fresh, previous *IssueChildren) WebhookItemPatch {
	if p.Issue == nil || p.Issue.Children != nil {
		return p
	}
	kids := previous
	if fresh != nil && fresh.Total > 0 {
		kids = fresh
	}
	if kids == nil || kids.Total <= 0 {
		return p
	}
	issue := *p.Issue
	carried := *kids
	issue.Children = &carried
	p.Issue = &issue
	return p
}

// issueChildren is the Children the removed issue row carried, whichever
// list it was in; nil when it had none.
func (rem removedItem) issueChildren() *IssueChildren {
	if rem.issue != nil && rem.issue.Children != nil {
		return rem.issue.Children
	}
	if rem.workIssue != nil {
		return rem.workIssue.Children
	}
	return nil
}
