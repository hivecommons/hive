package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// PRIssueCounts holds the total merged-PR and closed-issue counts for a repo,
// used by the dashboard's Cost section to derive cost-per-PR and
// cost-per-issue efficiency metrics (issue #4110). Both counts are scoped to
// the configured hive author so the divisor matches the spend being audited by
// this hive rather than every human and historical PR in the repository.
type PRIssueCounts struct {
	MergedPRs    int    `json:"merged_prs"`
	ClosedIssues int    `json:"closed_issues"`
	UpdatedAt    string `json:"updated_at"`
	Author       string `json:"author,omitempty"`
	Basis        string `json:"basis,omitempty"`
	WindowStart  string `json:"window_start,omitempty"`
}

// ComputePRIssueCounts fetches the hive-attributed number of merged pull
// requests and closed issues for the given repo via the GitHub Search API. The
// author is the configured hive actor (usually a GitHub App bot), keeping the
// per-unit tiles auditable: estimated hive spend ÷ hive-authored outcomes.
func (c *Client) ComputePRIssueCounts(ctx context.Context, repo, author string) (*PRIssueCounts, error) {
	return c.ComputePRIssueCountsSince(ctx, repo, author, time.Time{})
}

// ComputePRIssueCountsSince is ComputePRIssueCounts scoped to outcomes that
// landed no earlier than since. A zero since preserves the historical all-time
// behavior.
func (c *Client) ComputePRIssueCountsSince(ctx context.Context, repo, author string, since time.Time) (*PRIssueCounts, error) {
	owner, repoName := c.splitRepo(repo)
	author = strings.TrimSpace(author)
	mergedQualifier := fmt.Sprintf("repo:%s/%s type:pr is:merged", owner, repoName)
	closedQualifier := fmt.Sprintf("repo:%s/%s type:issue is:closed", owner, repoName)
	windowStart := ""
	if !since.IsZero() {
		day := since.UTC().Format("2006-01-02")
		mergedQualifier += " merged:>=" + day
		closedQualifier += " closed:>=" + day
		windowStart = since.UTC().Format(time.RFC3339)
	}
	if author == "" {
		return &PRIssueCounts{
			UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
			Basis:       "hive-attributed",
			WindowStart: windowStart,
		}, nil
	}

	merged, err := c.searchAuthorTotal(ctx, mergedQualifier, author)
	if err != nil {
		return nil, fmt.Errorf("counting merged PRs for %s/%s: %w", owner, repoName, err)
	}

	closed, err := c.searchAuthorTotal(ctx, closedQualifier, author)
	if err != nil {
		return nil, fmt.Errorf("counting closed issues for %s/%s: %w", owner, repoName, err)
	}

	return &PRIssueCounts{
		MergedPRs:    merged,
		ClosedIssues: closed,
		UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
		Author:       author,
		Basis:        "hive-attributed",
		WindowStart:  windowStart,
	}, nil
}

func (c *Client) searchAuthorTotal(ctx context.Context, baseQualifier, author string) (int, error) {
	best := 0
	for _, q := range authorQualifiers(author) {
		total, err := c.searchTotal(ctx, baseQualifier+" "+q)
		if err != nil {
			return 0, err
		}
		if total > best {
			best = total
		}
	}
	return best, nil
}

func authorQualifiers(author string) []string {
	author = strings.TrimSpace(author)
	if author == "" {
		return nil
	}
	if strings.HasSuffix(strings.ToLower(author), "[bot]") {
		return []string{"author:" + author}
	}
	return []string{"author:" + author, "author:app/" + author}
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
	// Search can return HTTP 200 with partial results when GitHub cannot
	// finish the query. Such a total (often zero) must not replace the
	// dashboard's persisted last-good cost divisors.
	if result == nil || result.Total == nil || result.GetIncompleteResults() {
		return 0, fmt.Errorf("search returned an incomplete or missing total")
	}
	return result.GetTotal(), nil
}
