package github

import (
	"context"
	"fmt"
)

// IssueClaimBlockReason checks current forge labels before an automated claim.
// Like a hold, needs-human prevents new claims and renewals, even when forced.
// Lookup errors are returned so callers cannot mistake unknown labels for
// permission to claim. Human claims do not use this gate.
func (c *Client) IssueClaimBlockReason(ctx context.Context, repo string, number int) (string, error) {
	if c == nil || c.client == nil {
		return "", ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	issue, _, err := c.client.Issues.Get(ctx, owner, name, number)
	if err != nil {
		return "", fmt.Errorf("checking issue claim labels: %w", err)
	}
	labels := extractLabels(issue.Labels)
	if hasIssueNeedsHumanLabel(labels) {
		return "needs-human", nil
	}
	if c.isHeld(labels) {
		return "hold", nil
	}
	return "", nil
}

// issueIsPullRequest reports whether repo#number is a pull request. The
// comment relay's needs-decision park uses it to refuse PR targets (#12334):
// the needs-decision label is an issue-queue signal, and on a PR it would
// hard-suppress the PR instead. A lookup error is returned so the caller
// retries rather than parking an item it could not read.
func (c *Client) issueIsPullRequest(ctx context.Context, repo string, number int) (bool, error) {
	if c == nil || c.client == nil {
		return false, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	issue, _, err := c.client.Issues.Get(ctx, owner, name, number)
	if err != nil {
		return false, fmt.Errorf("reading %s#%d before parking it: %w", repo, number, err)
	}
	return issue.IsPullRequest(), nil
}
