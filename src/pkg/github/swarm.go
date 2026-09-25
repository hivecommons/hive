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
	IssuesClosed     int
	PRsMerged        int
	SpeksCompleted   int
	LocalModelPRs    int
	Participants     []string
	PRsByAuthor      map[string]int
	IssuesClosedBy   map[string]int
	SpeksCompletedBy map[string]int
	LocalModelPRsBy  map[string]int
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
	closed, issuesByCloser, err := c.searchClosedSwarmIssues(ctx, repo, start, end)
	if err != nil {
		return SwarmScore{}, fmt.Errorf("counting closed issues: %w", err)
	}
	merged, participants, prsByAuthor, speksCompleted, localModelPRs, speksByAuthor, localByAuthor, err := c.searchMergedPRs(ctx, repo, start, end)
	if err != nil {
		return SwarmScore{}, fmt.Errorf("counting merged PRs: %w", err)
	}
	return SwarmScore{IssuesClosed: closed, PRsMerged: merged, SpeksCompleted: speksCompleted, LocalModelPRs: localModelPRs, Participants: participants, PRsByAuthor: prsByAuthor, IssuesClosedBy: issuesByCloser, SpeksCompletedBy: speksByAuthor, LocalModelPRsBy: localByAuthor}, nil
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

func (c *Client) searchClosedSwarmIssues(ctx context.Context, repo string, start, end time.Time) (int, map[string]int, error) {
	query := fmt.Sprintf("repo:%s is:issue is:closed closed:%s..%s", repo, swarmSearchTime(start), swarmSearchTime(end))
	result, _, err := c.client.Search.Issues(ctx, query, &gh.SearchOptions{ListOptions: gh.ListOptions{PerPage: swarmSearchPerPage}})
	if err != nil {
		return 0, nil, err
	}
	byCloser := map[string]int{}
	for _, item := range result.Issues {
		if item == nil || item.ClosedBy == nil {
			continue
		}
		login := strings.TrimSpace(item.ClosedBy.GetLogin())
		if login != "" {
			byCloser[login]++
		}
	}
	return result.GetTotal(), byCloser, nil
}

func (c *Client) searchMergedPRs(ctx context.Context, repo string, start, end time.Time) (int, []string, map[string]int, int, int, map[string]int, map[string]int, error) {
	query := fmt.Sprintf("repo:%s is:pr is:merged merged:%s..%s", repo, swarmSearchTime(start), swarmSearchTime(end))
	result, _, err := c.client.Search.Issues(ctx, query, &gh.SearchOptions{ListOptions: gh.ListOptions{PerPage: swarmSearchPerPage}})
	if err != nil {
		return 0, nil, nil, 0, 0, nil, nil, err
	}
	seen := map[string]bool{}
	byAuthor := map[string]int{}
	speksByAuthor := map[string]int{}
	localByAuthor := map[string]int{}
	speksCompleted := 0
	localModelPRs := 0
	for _, item := range result.Issues {
		if item == nil || item.User == nil {
			continue
		}
		login := strings.TrimSpace(item.User.GetLogin())
		if login != "" {
			seen[login] = true
			byAuthor[login]++
		}
		body := item.GetBody()
		text := strings.ToLower(item.GetTitle() + "\n" + body)
		if swarmPRCompletesSpek(text) {
			speksCompleted++
			if login != "" {
				speksByAuthor[login]++
			}
		}
		if swarmPRUsesLocalModel(body) {
			localModelPRs++
			if login != "" {
				localByAuthor[login]++
			}
		}
	}
	participants := make([]string, 0, len(seen))
	for login := range seen {
		participants = append(participants, login)
	}
	sort.Strings(participants)
	return result.GetTotal(), participants, byAuthor, speksCompleted, localModelPRs, speksByAuthor, localByAuthor, nil
}

func swarmPRCompletesSpek(text string) bool {
	return strings.Contains(text, "spek") || (strings.Contains(text, "spec") && strings.Contains(text, "plan") && strings.Contains(text, "implement"))
}

func swarmPRUsesLocalModel(body string) bool {
	meta, ok := ParseAttributionTrailer(body)
	return ok && swarmAttributionIsLocal(meta)
}

func swarmAttributionIsLocal(meta InvocationMeta) bool {
	backend := strings.ToLower(strings.TrimSpace(meta.Backend))
	model := strings.ToLower(strings.TrimSpace(meta.Model))
	switch backend {
	case "bob", "ollama", "local":
		return true
	default:
		return strings.HasPrefix(backend, "ollama") || strings.HasPrefix(model, "local")
	}
}

func swarmSearchTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
