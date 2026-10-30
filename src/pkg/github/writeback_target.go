package github

import (
	"context"
	"fmt"

	"github.com/hivecommons/hive/pkg/writeback"
)

// WritebackItemForNumber reads policy metadata when a writer has only an item
// reference, rather than an enumerated Issue or PullRequest.
func (c *Client) WritebackItemForNumber(ctx context.Context, repo string, number int) (writeback.Item, error) {
	if c == nil || c.client == nil {
		return writeback.Item{}, ErrNoGitHubClient
	}
	if c.WritebackGate() == nil {
		return writeback.Item{Number: number}, nil
	}
	owner, name := c.splitRepo(repo)
	issue, _, err := c.client.Issues.Get(ctx, owner, name, number)
	if err != nil {
		return writeback.Item{}, fmt.Errorf("reading writeback target %s#%d: %w", repo, number, err)
	}
	if issue.IsPullRequest() {
		_, attributed := ParseAttributionTrailer(issue.GetBody())
		return c.WritebackItemForPR(PullRequest{
			Number: number, Author: safeGetLogin(issue.GetUser()), HiveAttributed: attributed,
			Labels: issueLabelNames(issue.Labels), CreatedAt: issue.GetCreatedAt().Time,
		}), nil
	}
	return c.WritebackItemForIssue(Issue{
		Number: number, Author: safeGetLogin(issue.GetUser()),
		Labels: issueLabelNames(issue.Labels), CreatedAt: issue.GetCreatedAt().Time,
	}), nil
}
