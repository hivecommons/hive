package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// MergedPRBackfillWindow is how far back the startup lifecycle backfill
// looks for merged PRs (hivecommons/hive#12007). Issue-side pr_opened/merged
// stages only began recording with #11880, so a freshly upgraded spoke needs
// the last week of merges folded onto their Fixes issues to stop reporting
// already-closed work as stale in-progress.
const MergedPRBackfillWindow = 7 * 24 * time.Hour

// mergedPRClosingIssues returns the issues pr closes with a closing keyword
// (Fixes/Closes/Resolves #N) in its title or body, restricted to the PR's own
// repository — a bare #N or an explicit "owner/repo#N" naming it. The PR's
// own number is never returned.
func mergedPRClosingIssues(pr *gh.PullRequest, repo, fullRepo string) []int {
	if pr == nil {
		return nil
	}
	var out []int
	seen := map[int]bool{}
	for _, ref := range ParseClaimedIssues(pr.GetTitle()+"\n"+pr.GetBody(), repo) {
		if !strings.EqualFold(ref.Repo, repo) && !strings.EqualFold(ref.Repo, fullRepo) {
			continue
		}
		if ref.Issue <= 0 || ref.Issue == pr.GetNumber() || seen[ref.Issue] {
			continue
		}
		seen[ref.Issue] = true
		out = append(out, ref.Issue)
	}
	return out
}

// mergedPRObservation projects a merged PR into the merged terminal
// observation the lifecycle backfill consumes. ok is false for a PR that did
// not merge.
func mergedPRObservation(pr *gh.PullRequest, repo, fullRepo string) (PRTerminalObservation, bool) {
	if pr == nil {
		return PRTerminalObservation{}, false
	}
	mergedAt := pr.GetMergedAt().Time
	if mergedAt.IsZero() {
		return PRTerminalObservation{}, false
	}
	return PRTerminalObservation{
		Repo:       repo,
		Number:     pr.GetNumber(),
		State:      PRStateMerged,
		ObservedAt: mergedAt,
		Actor:      safeGetLogin(pr.GetMergedBy()),
		CreatedAt:  pr.GetCreatedAt().Time,
		URL:        pr.GetHTMLURL(),
		Issues:     mergedPRClosingIssues(pr, repo, fullRepo),
	}, true
}

// MergedPRsSince lists the PRs merged at or after since in every active repo
// as merged terminal observations carrying the PR's created_at and the
// issues it closes. One closed-PR listing per repo, sorted by update recency
// and stopped at the cutoff (the FetchClaimScan settle-scan shape), bounded
// by claimSearchMaxPages. A per-repo failure is reported via err while the
// other repos' results are still returned.
func (c *Client) MergedPRsSince(ctx context.Context, since time.Time) ([]PRTerminalObservation, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	var out []PRTerminalObservation
	var firstErr error
	for _, repo := range c.activeRepos() {
		owner, repoName := c.splitRepo(repo)
		opts := &gh.PullRequestListOptions{
			State:       "closed",
			Sort:        "updated",
			Direction:   "desc",
			ListOptions: gh.ListOptions{PerPage: claimSearchPerPage},
		}
		for page := 0; page < claimSearchMaxPages; page++ {
			prs, resp, err := c.client.PullRequests.List(WithRESTCaller(ctx, "hive:merged_pr_backfill"), owner, repoName, opts)
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("listing merged PRs for %s/%s: %w", owner, repoName, err)
				}
				break
			}
			pastWindow := false
			for _, pr := range prs {
				if pr == nil {
					continue
				}
				if pr.GetUpdatedAt().Time.Before(since) {
					pastWindow = true
					break
				}
				obs, ok := mergedPRObservation(pr, repo, owner+"/"+repoName)
				if !ok || obs.ObservedAt.Before(since) {
					continue
				}
				out = append(out, obs)
			}
			if pastWindow || resp == nil || resp.NextPage == 0 {
				break
			}
			opts.Page = resp.NextPage
		}
	}
	return out, firstErr
}
