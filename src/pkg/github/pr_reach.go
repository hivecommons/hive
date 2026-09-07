package github

import (
	"context"
	"fmt"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// ============================================================================
// PR facts for reach telemetry (#3994, phase 2b of #3973)
// ============================================================================
//
// The /api/reach join needs three GitHub facts per PR: the merge commit SHA,
// the merge time, and the changed-file list (input to the D3 PR→component
// mapping). All three are IMMUTABLE once a PR is merged, so they are cached
// permanently in bounded maps — the same drop-on-overflow discipline as
// pkg/hub's commitOrderCache, and the mutex+map+cachedAt shape of
// app_discovery.go. The recent-merged-PRs listing is the one answer that
// changes over time, so it alone carries a TTL.

// MergedPR is the merged-PR fact set the reach join consumes.
type MergedPR struct {
	Number         int
	Title          string
	MergeCommitSHA string
	MergedAt       time.Time
}

// mergedPRCacheMax / prFilesCacheMax bound the permanent caches of immutable
// merged-PR facts. PR numbers arrive from operator queries, so the key space
// is externally influenced and must not grow forever; on overflow the whole
// map is dropped (a re-fetch costs one API call per entry still in use —
// simpler and safer than an LRU, per commitOrderCacheMax in pkg/hub).
const (
	mergedPRCacheMax = 512
	prFilesCacheMax  = 512
)

// RecentMergedPRsTTL bounds how stale the recent-merged-PRs listing may be.
// Five minutes matches InstallationDiscoveryTTL: fresh enough for an
// operator-facing status endpoint, coarse enough that a dashboard poll never
// turns into an API call loop.
const RecentMergedPRsTTL = 5 * time.Minute

// recentMergedPRsMaxPages caps the closed-PR listing walk. At 100 PRs per
// page, five pages bounds the walk to 500 recently-updated closed PRs —
// far more than any recent-PRs query needs, and a hard stop against paging
// through the repo's whole history when few of them merged.
const recentMergedPRsMaxPages = 5

// listPerPage is the standard GitHub max page size, used by every paginated
// call in this file.
const listPerPage = 100

var (
	prReachCacheMu sync.Mutex
	// mergedPRCache / prFilesCache hold immutable merged-PR facts forever
	// (bounded above). Keyed "owner/repo#number".
	mergedPRCache = map[string]MergedPR{}
	prFilesCache  = map[string][]string{}
	// recentMergedCache holds the one time-varying answer, keyed
	// "owner/repo@base". Entries expire after RecentMergedPRsTTL.
	recentMergedCache = map[string]recentMergedEntry{}
)

type recentMergedEntry struct {
	prs      []MergedPR
	cachedAt time.Time
}

// prCacheKey builds the "owner/repo#number" cache key.
func prCacheKey(owner, repo string, number int) string {
	return fmt.Sprintf("%s/%s#%d", owner, repo, number)
}

// MergedPR fetches one PR's merged facts. It errors on a PR that is not
// merged — the reach join is defined only over merged code, and treating an
// open PR as merged would fabricate a merge time.
func (c *Client) MergedPR(ctx context.Context, owner, repo string, number int) (MergedPR, error) {
	if c == nil {
		return MergedPR{}, ErrNoGitHubClient
	}
	key := prCacheKey(owner, repo, number)
	prReachCacheMu.Lock()
	if cached, ok := mergedPRCache[key]; ok {
		prReachCacheMu.Unlock()
		return cached, nil
	}
	prReachCacheMu.Unlock()

	pr, _, err := c.client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return MergedPR{}, fmt.Errorf("fetching PR %s: %w", key, err)
	}
	if pr.MergedAt == nil || pr.GetMergeCommitSHA() == "" {
		return MergedPR{}, fmt.Errorf("PR %s is not merged", key)
	}
	merged := MergedPR{
		Number:         number,
		Title:          pr.GetTitle(),
		MergeCommitSHA: pr.GetMergeCommitSHA(),
		MergedAt:       pr.MergedAt.Time,
	}

	prReachCacheMu.Lock()
	if len(mergedPRCache) >= mergedPRCacheMax {
		mergedPRCache = map[string]MergedPR{}
	}
	mergedPRCache[key] = merged
	prReachCacheMu.Unlock()
	return merged, nil
}

// ListMergedPRFiles returns the changed-file paths of a MERGED PR (immutable,
// so cached permanently). It pages through the full list and then verifies
// completeness against the PR's own changed-file count — GitHub truncates the
// files endpoint on very large PRs, and a silently short list would
// under-attribute components and understate reach (same guard as
// fetchIntentPREvidence in cmd/hive).
func (c *Client) ListMergedPRFiles(ctx context.Context, owner, repo string, number int) ([]string, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	key := prCacheKey(owner, repo, number)
	prReachCacheMu.Lock()
	if cached, ok := prFilesCache[key]; ok {
		prReachCacheMu.Unlock()
		return cached, nil
	}
	prReachCacheMu.Unlock()

	pr, _, err := c.client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("fetching PR %s: %w", key, err)
	}
	if pr.MergedAt == nil {
		return nil, fmt.Errorf("PR %s is not merged; refusing to cache a mutable file list", key)
	}

	var files []string
	opts := &gh.ListOptions{PerPage: listPerPage}
	for {
		page, resp, err := c.client.PullRequests.ListFiles(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("listing PR %s files: %w", key, err)
		}
		for _, f := range page {
			files = append(files, f.GetFilename())
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	if reported := pr.GetChangedFiles(); reported > len(files) {
		return nil, fmt.Errorf("incomplete PR file list for %s: GitHub reported %d changed files but the API returned %d — refusing a partial attribution", key, reported, len(files))
	}

	prReachCacheMu.Lock()
	if len(prFilesCache) >= prFilesCacheMax {
		prFilesCache = map[string][]string{}
	}
	prFilesCache[key] = files
	prReachCacheMu.Unlock()
	return files, nil
}

// RecentMergedPRs lists up to limit PRs merged into base, newest merge first,
// cached for RecentMergedPRsTTL. It walks closed PRs sorted by update
// recency and keeps only real
// merges into the requested base branch.
func (c *Client) RecentMergedPRs(ctx context.Context, owner, repo, base string, limit int) ([]MergedPR, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	if limit <= 0 {
		return nil, nil
	}
	cacheKey := fmt.Sprintf("%s/%s@%s", owner, repo, base)
	prReachCacheMu.Lock()
	if entry, ok := recentMergedCache[cacheKey]; ok && time.Since(entry.cachedAt) < RecentMergedPRsTTL && len(entry.prs) >= limit {
		result := entry.prs[:limit]
		prReachCacheMu.Unlock()
		return result, nil
	}
	prReachCacheMu.Unlock()

	var merged []MergedPR
	opts := &gh.PullRequestListOptions{
		State:       "closed",
		Base:        base,
		Sort:        "updated",
		Direction:   "desc",
		ListOptions: gh.ListOptions{PerPage: listPerPage},
	}
	for page := 0; page < recentMergedPRsMaxPages && len(merged) < limit; page++ {
		prs, resp, err := c.client.PullRequests.List(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("listing merged PRs for %s: %w", cacheKey, err)
		}
		for _, pr := range prs {
			if pr.MergedAt == nil || pr.GetMergeCommitSHA() == "" {
				continue // closed without merging
			}
			merged = append(merged, MergedPR{
				Number:         pr.GetNumber(),
				Title:          pr.GetTitle(),
				MergeCommitSHA: pr.GetMergeCommitSHA(),
				MergedAt:       pr.MergedAt.Time,
			})
			if len(merged) >= limit {
				break
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	prReachCacheMu.Lock()
	if len(merged) > 0 {
		recentMergedCache[cacheKey] = recentMergedEntry{prs: merged, cachedAt: time.Now()}
	}
	prReachCacheMu.Unlock()
	return merged, nil
}
