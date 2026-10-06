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
