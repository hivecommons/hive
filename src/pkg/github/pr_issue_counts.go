package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
	Stale        bool   `json:"stale,omitempty"`
	// StaleSince is when the counts stopped being confirmed by GitHub (RFC
	// 3339); set with Stale when the last-good counts are being served so
	// the dashboard can say "stale since <t>" instead of showing zeros
	// (hivecommons/hive#13376).
	StaleSince string `json:"stale_since,omitempty"`
	Status     string `json:"status,omitempty"`
	// Source is "search" when the counts came straight from the Search API,
	// "forgestate" when they are the cache's webhook-maintained counters
	// (reconciled against search hourly).
	Source string `json:"source,omitempty"`
	// Partial is set when some of the batched queries failed: the totals
	// cover the repos whose queries succeeded and undercount the rest.
	// Error carries the failure so the dashboard can say why instead of
	// showing "—" (projectbluefin's 19-repo hive never produced a count).
	Partial bool   `json:"partial,omitempty"`
	Error   string `json:"error,omitempty"`
}

// searchQueryMaxLen is GitHub's limit on a search query's length; repo
// qualifiers are batched into as few queries as fit under it.
const searchQueryMaxLen = 256

// ComputePRIssueCounts fetches the hive-attributed number of merged pull
// requests and closed issues for the given repo via the GitHub Search API. The
// author is the configured hive actor (usually a GitHub App bot), keeping the
// per-unit tiles auditable: estimated hive spend ÷ hive-authored outcomes.
func (c *Client) ComputePRIssueCounts(ctx context.Context, repo, author string) (*PRIssueCounts, error) {
	return c.ComputePRIssueCountsSince(ctx, repo, author, time.Time{})
}

// ComputePRIssueCountsForRepos aggregates ComputePRIssueCounts across every
// configured repository.
func (c *Client) ComputePRIssueCountsForRepos(ctx context.Context, repos []string, author string) (*PRIssueCounts, error) {
	return c.ComputePRIssueCountsForReposSince(ctx, repos, author, time.Time{})
}

// ComputePRIssueCountsSince is ComputePRIssueCounts scoped to outcomes that
// landed no earlier than since. A zero since preserves the historical all-time
// behavior.
func (c *Client) ComputePRIssueCountsSince(ctx context.Context, repo, author string, since time.Time) (*PRIssueCounts, error) {
	return c.ComputePRIssueCountsForReposSince(ctx, []string{repo}, author, since)
}

// ComputePRIssueCountsForReposSince aggregates ComputePRIssueCountsSince across
// every configured repository.
func (c *Client) ComputePRIssueCountsForReposSince(ctx context.Context, repos []string, author string, since time.Time) (*PRIssueCounts, error) {
	repos = uniqueNonEmpty(repos)
	author = strings.TrimSpace(author)
	windowStart := ""
	if !since.IsZero() {
		windowStart = since.UTC().Format(time.RFC3339)
	}
	if author == "" {
		return &PRIssueCounts{
			UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
			Basis:       "hive-attributed",
			WindowStart: windowStart,
			Status:      "unavailable: missing hive author",
		}, nil
	}

	day := ""
	if !since.IsZero() {
		day = since.UTC().Format("2006-01-02")
	}
	qualified := make([]string, 0, len(repos))
	for _, repo := range repos {
		owner, repoName := c.splitRepo(repo)
		qualified = append(qualified, "repo:"+owner+"/"+repoName)
	}

	var merged, closed int
	var errs []error
	failed := 0
	count := func(kind string, qualifier string) (int, error) {
		suffix := " type:" + kind
		switch kind {
		case "pr":
			suffix += " is:merged"
			if day != "" {
				suffix += " merged:>=" + day
			}
		default:
			suffix += " is:closed"
			if day != "" {
				suffix += " closed:>=" + day
			}
		}
		return c.searchAuthorTotal(ctx, qualifier+suffix, author)
	}
	// One query per batch of repos (instead of one per repo) keeps a
	// 19-repo hive inside search's 30 calls/minute; a batch that fails is
	// recorded and the rest still count so a single refusal no longer
	// discards everything.
	batches := batchRepoQualifiers(qualified, searchQueryMaxLen-len(" type:issue is:closed closed:>=2006-01-02 ")-longestAuthorQualifier(author))
	rateLimited := false
	for _, batch := range batches {
		for _, kind := range []string{"pr", "issue"} {
			if rateLimited {
				failed++
				continue
			}
			total, err := count(kind, batch)
			if err != nil {
				failed++
				errs = append(errs, fmt.Errorf("counting %s outcomes for %s: %w", kind, batch, err))
				if isSearchRateLimit(err) {
					rateLimited = true
				}
				continue
			}
			if kind == "pr" {
				merged += total
			} else {
				closed += total
			}
		}
	}
	attempted := 2 * len(batches)
	if failed == attempted && attempted > 0 {
		return nil, errors.Join(errs...)
	}

	result := &PRIssueCounts{
		MergedPRs:    merged,
		ClosedIssues: closed,
		UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
		Author:       author,
		Basis:        "hive-attributed",
		WindowStart:  windowStart,
		Status:       "fresh",
		Source:       "search",
	}
	if failed > 0 {
		result.Partial = true
		result.Error = errors.Join(errs...).Error()
		result.Status = fmt.Sprintf("partial: %d of %d search queries failed", failed, attempted)
	}
	return result, nil
}

// batchRepoQualifiers packs repo: qualifiers into space-separated groups
// whose length stays under limit, so each group is one search query. A
// qualifier longer than the limit on its own still gets its own group.
func batchRepoQualifiers(qualifiers []string, limit int) []string {
	var out []string
	current := ""
	for _, q := range qualifiers {
		switch {
		case current == "":
			current = q
		case len(current)+1+len(q) <= limit:
			current += " " + q
		default:
			out = append(out, current)
			current = q
		}
	}
	if current != "" {
		out = append(out, current)
	}
	return out
}

func longestAuthorQualifier(author string) int {
	longest := 0
	for _, q := range authorQualifiers(author) {
		if len(q) > longest {
			longest = len(q)
		}
	}
	return longest
}

// isSearchRateLimit reports whether err is GitHub pacing us (primary or
// secondary rate limit), in which case the remaining queries of this count
// are skipped instead of each failing the same way.
func isSearchRateLimit(err error) bool {
	var rl *gh.RateLimitError
	var abuse *gh.AbuseRateLimitError
	if errors.As(err, &rl) || errors.As(err, &abuse) {
		return true
	}
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "rate limit")
}

// searchAuthorTotal returns the best total across the author qualifiers. A
// qualifier GitHub rejects as unsearchable (422, e.g. author:app/<login> when
// no such App exists) contributes zero; the error is only returned when every
// qualifier is rejected. Any other error fails the count immediately.
func (c *Client) searchAuthorTotal(ctx context.Context, baseQualifier, author string) (int, error) {
	best := 0
	var rejected error
	accepted := 0
	for _, q := range authorQualifiers(author) {
		total, err := c.searchTotal(ctx, baseQualifier+" "+q)
		if err != nil {
			var ghErr *gh.ErrorResponse
			if errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusUnprocessableEntity {
				rejected = err
				continue
			}
			return 0, err
		}
		accepted++
		if total > 0 {
			return total, nil
		}
		if total > best {
			best = total
		}
	}
	if accepted == 0 && rejected != nil {
		return 0, rejected
	}
	return best, nil
}

func authorQualifiers(author string) []string {
	author = strings.TrimSpace(author)
	if author == "" {
		return nil
	}
	if strings.HasSuffix(strings.ToLower(author), "[bot]") {
		slug := strings.TrimSuffix(author, "[bot]")
		return []string{"author:app/" + slug, "author:" + author}
	}
	return []string{"author:app/" + author, "author:" + author}
}

func uniqueNonEmpty(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	return out
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
