package github

import (
	"context"
	"errors"
	"fmt"
)

// Empty-issue-list verification (hosted hive, 2026-10-22 incident).
//
// EnumerateActionable lists a repo's issues and then its pull requests. The
// issue list returning an empty page is not an error: GitHub answers 200 []
// for a repo with no open issues, but also for a credential that may read
// pull requests and not issues (the issues feed then carries only the
// pull-request rows, which the classifier skips), and a cache or list that
// under-reports looks the same. Every such pass used to publish the repo as
// "0 issues · N PRs" with no indication anything was missing, across every
// repo at once, while the governor saw an empty queue.
//
// A repo whose issue list is empty while its PR list is not is therefore
// checked against the repository record before it is published: GitHub's
// open_issues_count includes open pull requests, so anything above the PR
// total is open issues the list did not return.

// errIssueListIncomplete marks an issue list that returned fewer open
// issues than the repository record says exist. It is transient for the
// last-good substitution: the list, not the repo, is what failed.
var errIssueListIncomplete = errors.New("issue list incomplete")

// verifyEmptyIssueList is called when a repo listed prTotal open pull
// requests and no open issues. It returns how many open issues the
// repository record reports beyond the pull requests (0 when the empty list
// is consistent with the record) or the error that stopped the check.
func (c *Client) verifyEmptyIssueList(ctx context.Context, repo string, prTotal int) (missing int, err error) {
	owner, repoName := c.splitRepo(repo)
	rec, _, err := c.client.Repositories.Get(ctx, owner, repoName)
	if err != nil {
		return 0, fmt.Errorf("verifying empty issue list for %s/%s: %w", owner, repoName, err)
	}
	return max(0, rec.GetOpenIssuesCount()-prTotal), nil
}
