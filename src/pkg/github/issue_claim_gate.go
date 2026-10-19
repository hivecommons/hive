package github

import (
	"context"
	"fmt"
)

// ClaimBlockReasonPullRequest is the IssueClaimBlockReason for a number that
// names a pull request. Issue claims (the ledger, the `claim` relay request,
// the contributor lane) stage an item in the issue family — stage/020,
// stage/030 — and a pull request must never carry one (ADR-0025: the
// families are disjoint). A PR's stage is the PR sweep's alone.
const ClaimBlockReasonPullRequest = "pull request"

// IssueClaimBlockReason checks current forge labels before an automated claim.
// Like a hold, needs-human prevents new claims and renewals, even when forced,
// and a pull request is never claimable as an issue
// (ClaimBlockReasonPullRequest). Lookup errors are returned so callers cannot
// mistake unknown labels for permission to claim. Human claims do not use
// this gate.
func (c *Client) IssueClaimBlockReason(ctx context.Context, repo string, number int) (string, error) {
	if c == nil || c.client == nil {
		return "", ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	issue, _, err := c.client.Issues.Get(ctx, owner, name, number)
	if err != nil {
		return "", fmt.Errorf("checking issue claim labels: %w", err)
	}
	if issue.IsPullRequest() {
		return ClaimBlockReasonPullRequest, nil
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
