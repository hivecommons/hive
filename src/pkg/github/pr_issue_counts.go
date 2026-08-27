package github

import (
	"context"
	"fmt"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// PRIssueCounts holds the total merged-PR and closed-issue counts for a repo,
// used by the dashboard's Cost section to derive cost-per-PR and
// cost-per-issue efficiency metrics (issue #4110). Both counts are all-time
// cumulative, matching the "estimated total — all-time cumulative" figure they
// are divided into.
type PRIssueCounts struct {
	MergedPRs    int    `json:"merged_prs"`
	ClosedIssues int    `json:"closed_issues"`
	UpdatedAt    string `json:"updated_at"`
}

// ComputePRIssueCounts fetches the total number of merged pull requests and
// closed issues for the given repo via the GitHub Search API. It deliberately
// counts every merged PR / closed issue in the repo (not just ones authored by
// the AI agent) since the cost figure they're divided into is the spoke's
// total spend, not a per-author subset.
func (c *Client) ComputePRIssueCounts(ctx context.Context, repo string) (*PRIssueCounts, error) {
	owner, repoName := c.splitRepo(repo)

	merged, err := c.searchTotal(ctx, fmt.Sprintf("repo:%s/%s type:pr is:merged", owner, repoName))
	if err != nil {
		return nil, fmt.Errorf("counting merged PRs for %s/%s: %w", owner, repoName, err)
	}

	closed, err := c.searchTotal(ctx, fmt.Sprintf("repo:%s/%s type:issue is:closed", owner, repoName))
	if err != nil {
		return nil, fmt.Errorf("counting closed issues for %s/%s: %w", owner, repoName, err)
	}

	return &PRIssueCounts{
		MergedPRs:    merged,
		ClosedIssues: closed,
		UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// searchTotal runs a GitHub search-issues query and returns the reported total
// count. PerPage is 1 since only the total is needed, not the items.
func (c *Client) searchTotal(ctx context.Context, qualifier string) (int, error) {
	result, _, err := c.client.Search.Issues(ctx, qualifier, &gh.SearchOptions{
		ListOptions: gh.ListOptions{PerPage: 1},
	})
	if err != nil {
		return 0, err
	}
	return result.GetTotal(), nil
}
