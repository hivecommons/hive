package github

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// UpsertLifecycleIssue creates or updates the one GitHub issue carrying the
// repository-scoped lifecycle marker. Searching by marker before creation makes
// retries safe even when GitHub accepted a write before Hive persisted it.
func (c *Client) UpsertLifecycleIssue(ctx context.Context, repository, marker, title, body string, labels []string) (int, string, bool, error) {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return 0, "", false, err
	}
	if strings.TrimSpace(marker) == "" || strings.TrimSpace(title) == "" || strings.TrimSpace(body) == "" {
		return 0, "", false, fmt.Errorf("lifecycle marker, title, and body are required")
	}
	issueNumber, err := c.findLifecycleIssue(ctx, owner, repo, marker)
	if err != nil {
		return 0, "", false, err
	}
	request := &gh.IssueRequest{Title: gh.Ptr(title), Body: gh.Ptr(body), Labels: &labels, State: gh.Ptr("open")}
	if issueNumber > 0 {
		issue, _, err := c.client.Issues.Edit(ctx, owner, repo, issueNumber, request)
		if err != nil {
			return 0, "", false, fmt.Errorf("update lifecycle issue #%d: %w", issueNumber, err)
		}
		return issue.GetNumber(), issue.GetHTMLURL(), false, nil
	}
	issue, _, err := c.client.Issues.Create(ctx, owner, repo, request)
	if err != nil {
		return 0, "", false, fmt.Errorf("create lifecycle issue: %w", err)
	}
	return issue.GetNumber(), issue.GetHTMLURL(), true, nil
}

func (c *Client) UpdateLifecycleIssue(ctx context.Context, repository string, number int, title, body, state string, labels []string) (int, string, error) {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return 0, "", err
	}
	if number <= 0 || (state != "open" && state != "closed") {
		return 0, "", fmt.Errorf("valid lifecycle issue number and state are required")
	}
	request := &gh.IssueRequest{Title: gh.Ptr(title), Body: gh.Ptr(body), Labels: &labels, State: gh.Ptr(state)}
	issue, _, err := c.client.Issues.Edit(ctx, owner, repo, number, request)
	if err != nil {
		return 0, "", fmt.Errorf("update lifecycle issue #%d: %w", number, err)
	}
	return issue.GetNumber(), issue.GetHTMLURL(), nil
}

func (c *Client) findLifecycleIssue(ctx context.Context, owner, repo, marker string) (int, error) {
	page := 1
	for page > 0 {
		issues, response, err := c.client.Issues.ListByRepo(ctx, owner, repo, &gh.IssueListByRepoOptions{
			State: "all", ListOptions: gh.ListOptions{Page: page, PerPage: 100},
		})
		if err != nil {
			return 0, fmt.Errorf("list lifecycle issues: %w", err)
		}
		for _, issue := range issues {
			if issue.IsPullRequest() {
				continue
			}
			if strings.Contains(issue.GetBody(), marker) {
				return issue.GetNumber(), nil
			}
		}
		page = response.NextPage
	}
	return 0, nil
}

func splitFullRepository(repository string) (string, string, error) {
	parts := strings.Split(strings.TrimSpace(repository), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("repository must be owner/name")
	}
	return parts[0], parts[1], nil
}
