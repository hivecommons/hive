package github

import (
	"context"
	"fmt"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// FleetContribCounts holds the spoke-computed contribution totals a hive
// reports to the hub for the public landing-page "live fleet stats" strip.
// These are counts of the AI author's PRs across the hive's own org, so the
// hub can aggregate a true fleet total across every registered spoke.
type FleetContribCounts struct {
	// PRsMerged is the number of PRs the AI author has merged in the window.
	PRsMerged int `json:"prs_merged"`
	// PRsRejected is the number of PRs the AI author opened that were closed
	// without merging in the window — the pipeline (build/lint/test/policy)
	// rejected them. This is the honest "the gate caught something" signal.
	PRsRejected int `json:"prs_rejected"`
	// CVEsClosed is the number of merged PRs by the AI author whose title or
	// body references a CVE id (matched server-side by GitHub search). It is
	// all-time rather than windowed, since CVE fixes are a cumulative claim.
	CVEsClosed int `json:"cves_closed"`
}

// fleetStatsWindowDays is the trailing window (in days) over which merged and
// rejected PR counts are measured. Ninety days keeps the number current
// ("what has the fleet done lately") without being a lifetime figure that only
// ever grows and stops meaning anything.
const fleetStatsWindowDays = 90

// searchIssueTotal runs a GitHub issue/PR search and returns only the total
// match count (PerPage 1 — we never page, GitHub returns the total in the
// first response). Shared by the fleet-stat counters below.
func (c *Client) searchIssueTotal(ctx context.Context, qualifier string) (int, error) {
	result, _, err := c.client.Search.Issues(ctx, qualifier, &gh.SearchOptions{
		ListOptions: gh.ListOptions{PerPage: 1},
	})
	if err != nil {
		return 0, fmt.Errorf("searching %q: %w", qualifier, err)
	}
	return result.GetTotal(), nil
}

// SearchMergedPRCountSince counts PRs by author within org merged on or after
// the given cutoff date. Used for the fleet "PRs merged" stat.
func (c *Client) SearchMergedPRCountSince(ctx context.Context, author, org string, since time.Time) (int, error) {
	q := fmt.Sprintf("type:pr author:%s org:%s is:merged merged:>=%s",
		author, org, since.UTC().Format("2006-01-02"))
	return c.searchIssueTotal(ctx, q)
}

// SearchRejectedPRCountSince counts PRs by author within org that were closed
// WITHOUT merging on or after the cutoff date — i.e. the pipeline rejected
// them. Used for the fleet "PRs rejected" stat.
func (c *Client) SearchRejectedPRCountSince(ctx context.Context, author, org string, since time.Time) (int, error) {
	q := fmt.Sprintf("type:pr author:%s org:%s is:closed is:unmerged closed:>=%s",
		author, org, since.UTC().Format("2006-01-02"))
	return c.searchIssueTotal(ctx, q)
}

// SearchCVEPRCount counts merged PRs by author within org whose title or body
// references a CVE id. GitHub's search treats "CVE" as a term match, so the
// count is an upper bound the hub reports honestly as "PRs referencing a CVE".
func (c *Client) SearchCVEPRCount(ctx context.Context, author, org string) (int, error) {
	// The quoted "CVE-" term biases the free-text match toward real CVE ids
	// (CVE-YYYY-N…) rather than incidental uses of the word "cve".
	q := fmt.Sprintf("type:pr author:%s org:%s is:merged \"CVE-\"", author, org)
	return c.searchIssueTotal(ctx, q)
}

// ComputeFleetContribCounts computes the three fleet-stat contribution counts
// for this hive's AI author across its org. It is a read-only aggregation over
// GitHub search; callers should invoke it on a timer and cache the result
// rather than per-heartbeat, since each call is three search API requests.
//
// An empty author or org yields a zero-value result and no error — a
// misconfigured hive simply contributes nothing to the fleet total rather than
// failing the heartbeat.
func (c *Client) ComputeFleetContribCounts(ctx context.Context, author, org string) (FleetContribCounts, error) {
	var out FleetContribCounts
	if c == nil || author == "" || org == "" {
		return out, nil
	}
	since := time.Now().AddDate(0, 0, -fleetStatsWindowDays)

	merged, err := c.SearchMergedPRCountSince(ctx, author, org, since)
	if err != nil {
		return out, fmt.Errorf("fleet merged count: %w", err)
	}
	out.PRsMerged = merged

	rejected, err := c.SearchRejectedPRCountSince(ctx, author, org, since)
	if err != nil {
		return out, fmt.Errorf("fleet rejected count: %w", err)
	}
	out.PRsRejected = rejected

	cves, err := c.SearchCVEPRCount(ctx, author, org)
	if err != nil {
		return out, fmt.Errorf("fleet cve count: %w", err)
	}
	out.CVEsClosed = cves

	return out, nil
}
