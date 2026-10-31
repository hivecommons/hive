package github

import (
	"context"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// IssueSnapshot is one issue as GitHub reports it at list time, independent
// of the enumeration's actionable filtering: open or closed, held or not.
// Repo is always owner/name. It is the raw input of transition detectors
// (pkg/rfcnotify) that need to see an issue leave the actionable set, which
// ActionableResult by construction cannot show.
type IssueSnapshot struct {
	Repo   string
	Number int
	Title  string
	URL    string
	Author string
	// Body is the issue's markdown description.
	Body string
	// State is "open" or "closed".
	State string
	// StateReason is GitHub's state_reason: "completed", "not_planned",
	// "reopened" or "" when GitHub did not report one.
	StateReason string
	Labels      []string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ClosedAt    time.Time
}

// IssueSnapshotOptions selects the issues ListIssueSnapshots returns.
type IssueSnapshotOptions struct {
	// State is "open", "closed" or "all" (empty means "open").
	State string
	// Since, when set, returns only issues updated at or after it.
	Since time.Time
}

// ListIssueSnapshots lists issues (never pull requests) across every managed
// repo, newest activity first, paging through GitHub's list endpoint. It
// stops at the first repo that fails so a caller never mistakes a partial
// read for a quiet period.
func (c *Client) ListIssueSnapshots(ctx context.Context, opts IssueSnapshotOptions) ([]IssueSnapshot, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	state := strings.ToLower(strings.TrimSpace(opts.State))
	if state == "" {
		state = "open"
	}
	var out []IssueSnapshot
	for _, repo := range c.getRepos() {
		owner, repoName := c.splitRepo(repo)
		listOpts := &gh.IssueListByRepoOptions{
			State:       state,
			Since:       opts.Since,
			Sort:        "updated",
			Direction:   "desc",
			ListOptions: gh.ListOptions{PerPage: 100},
		}
		for {
			issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, repoName, listOpts)
			if err != nil {
				return nil, err
			}
			for _, issue := range issues {
				if issue == nil || issue.IsPullRequest() {
					continue
				}
				out = append(out, issueSnapshotFrom(owner+"/"+repoName, issue))
			}
			if resp == nil || resp.NextPage == 0 {
				break
			}
			listOpts.ListOptions.Page = resp.NextPage
		}
	}
	return out, nil
}

func issueSnapshotFrom(repo string, issue *gh.Issue) IssueSnapshot {
	return IssueSnapshot{
		Repo:        repo,
		Number:      issue.GetNumber(),
		Title:       issue.GetTitle(),
		URL:         issue.GetHTMLURL(),
		Author:      safeGetLogin(issue.GetUser()),
		Body:        issue.GetBody(),
		State:       strings.ToLower(issue.GetState()),
		StateReason: strings.ToLower(issue.GetStateReason()),
		Labels:      issueLabelNames(issue.Labels),
		CreatedAt:   issue.GetCreatedAt().Time,
		UpdatedAt:   issue.GetUpdatedAt().Time,
		ClosedAt:    issue.GetClosedAt().Time,
	}
}
