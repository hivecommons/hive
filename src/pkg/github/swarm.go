package github

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

const swarmSearchPerPage = 100

type SwarmScore struct {
	IssuesClosed int
	PRsMerged    int
	Participants []string
}

func BoostActionableRepoPriority(issues []Issue, repo string) {
	repo = strings.ToLower(strings.TrimSpace(repo))
	if repo == "" || len(issues) < 2 {
		return
	}
	sort.SliceStable(issues, func(i, j int) bool {
		left := issueRepoMatchesSwarm(issues[i].Repo, repo)
		right := issueRepoMatchesSwarm(issues[j].Repo, repo)
		return left && !right
	})
}

func issueRepoMatchesSwarm(issueRepo, swarmRepo string) bool {
	issueRepo = strings.ToLower(strings.TrimSpace(issueRepo))
	swarmRepo = strings.ToLower(strings.TrimSpace(swarmRepo))
	if issueRepo == "" || swarmRepo == "" {
		return false
	}
	if issueRepo == swarmRepo {
		return true
	}
	if idx := strings.IndexByte(swarmRepo, '/'); idx >= 0 {
		return issueRepo == swarmRepo[idx+1:]
	}
	return false
}

func (c *Client) ScoreSwarm(ctx context.Context, repo string, start, end time.Time) (SwarmScore, error) {
	if c == nil {
		return SwarmScore{}, ErrNoGitHubClient
	}
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return SwarmScore{}, fmt.Errorf("repo is required")
	}
	closed, err := c.searchSwarmTotal(ctx, fmt.Sprintf("repo:%s is:issue is:closed closed:%s..%s", repo, swarmSearchTime(start), swarmSearchTime(end)))
	if err != nil {
		return SwarmScore{}, fmt.Errorf("counting closed issues: %w", err)
	}
	merged, participants, err := c.searchMergedPRs(ctx, repo, start, end)
	if err != nil {
		return SwarmScore{}, fmt.Errorf("counting merged PRs: %w", err)
	}
	return SwarmScore{IssuesClosed: closed, PRsMerged: merged, Participants: participants}, nil
}

func (c *Client) CountUnlabeledOpenIssues(ctx context.Context, repo string) (int, error) {
	if c == nil {
		return 0, ErrNoGitHubClient
	}
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return 0, fmt.Errorf("repo is required")
	}
	return c.searchSwarmTotal(ctx, fmt.Sprintf("repo:%s is:issue is:open no:label", repo))
}

func (c *Client) searchSwarmTotal(ctx context.Context, query string) (int, error) {
	result, _, err := c.client.Search.Issues(ctx, query, &gh.SearchOptions{ListOptions: gh.ListOptions{PerPage: 1}})
	if err != nil {
		return 0, err
	}
	return result.GetTotal(), nil
}

func (c *Client) searchMergedPRs(ctx context.Context, repo string, start, end time.Time) (int, []string, error) {
	query := fmt.Sprintf("repo:%s is:pr is:merged merged:%s..%s", repo, swarmSearchTime(start), swarmSearchTime(end))
	result, _, err := c.client.Search.Issues(ctx, query, &gh.SearchOptions{ListOptions: gh.ListOptions{PerPage: swarmSearchPerPage}})
	if err != nil {
		return 0, nil, err
	}
	seen := map[string]bool{}
	for _, item := range result.Issues {
		if item == nil || item.User == nil {
			continue
		}
		login := strings.TrimSpace(item.User.GetLogin())
		if login != "" {
			seen[login] = true
		}
	}
	participants := make([]string, 0, len(seen))
	for login := range seen {
		participants = append(participants, login)
	}
	sort.Strings(participants)
	return result.GetTotal(), participants, nil
}

func swarmSearchTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
