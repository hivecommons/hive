package github

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

type RepairPullRequest struct {
	Number  int    `json:"number"`
	URL     string `json:"url"`
	HeadSHA string `json:"head_sha"`
	Created bool   `json:"created"`
}

// UpsertRepairPullRequest creates or updates exactly one open repair PR for a
// Hive-owned branch. The marker makes a retry after an ambiguous API response
// idempotent without relying on the local lifecycle transaction having
// completed.
func (c *Client) UpsertRepairPullRequest(ctx context.Context, repository, branch, base, title, body, marker string) (RepairPullRequest, error) {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return RepairPullRequest{}, err
	}
	if strings.TrimSpace(branch) == "" || strings.TrimSpace(base) == "" || strings.TrimSpace(marker) == "" {
		return RepairPullRequest{}, fmt.Errorf("repair branch, base branch, and marker are required")
	}
	pulls, _, err := c.client.PullRequests.List(ctx, owner, repo, &gh.PullRequestListOptions{
		State: "open", Head: owner + ":" + branch, Base: base, ListOptions: gh.ListOptions{PerPage: 100},
	})
	if err != nil {
		return RepairPullRequest{}, fmt.Errorf("list repair pull requests: %w", err)
	}
	var matched *gh.PullRequest
	for _, pull := range pulls {
		if strings.Contains(pull.GetBody(), marker) {
			if matched != nil {
				return RepairPullRequest{}, fmt.Errorf("multiple open repair pull requests contain marker %q", marker)
			}
			matched = pull
		}
	}
	if matched != nil {
		updated, _, err := c.client.PullRequests.Edit(ctx, owner, repo, matched.GetNumber(), &gh.PullRequest{Title: gh.Ptr(title), Body: gh.Ptr(body), Base: &gh.PullRequestBranch{Ref: gh.Ptr(base)}})
		if err != nil {
			return RepairPullRequest{}, fmt.Errorf("update repair pull request: %w", err)
		}
		return RepairPullRequest{Number: updated.GetNumber(), URL: updated.GetHTMLURL(), HeadSHA: updated.GetHead().GetSHA()}, nil
	}
	created, _, err := c.client.PullRequests.Create(ctx, owner, repo, &gh.NewPullRequest{
		Title: gh.Ptr(title), Head: gh.Ptr(branch), Base: gh.Ptr(base), Body: gh.Ptr(body), Draft: gh.Ptr(false), MaintainerCanModify: gh.Ptr(true),
	})
	if err != nil {
		return RepairPullRequest{}, fmt.Errorf("create repair pull request: %w", err)
	}
	return RepairPullRequest{Number: created.GetNumber(), URL: created.GetHTMLURL(), HeadSHA: created.GetHead().GetSHA(), Created: true}, nil
}
