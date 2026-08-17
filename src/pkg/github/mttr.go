package github

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// MTTRResult contains issue-to-merge time statistics.
type MTTRResult struct {
	AvgMinutes     int            `json:"avg_minutes"`
	MedianMinutes  int            `json:"median_minutes"`
	P90Minutes     int            `json:"p90_minutes"`
	Count          int            `json:"count"`
	FastestMinutes int            `json:"fastest_minutes"`
	SlowestMinutes int            `json:"slowest_minutes"`
	UpdatedAt      string         `json:"updated_at"`
	History        []MTTRBucket   `json:"history"`
}

// MTTRBucket is a time-bucketed MTTR data point for sparkline rendering.
type MTTRBucket struct {
	T      int64 `json:"t"`
	Avg    int   `json:"avg"`
	Median int   `json:"median"`
}

const (
	// mttrMergedPRLimit is the max number of recently merged PRs to scan.
	mttrMergedPRLimit = 100
	// mttrBucketHours is the time window for each sparkline data point.
	mttrBucketHours = 6
	// mttrBackfillDays limits how far back history buckets go.
	mttrBackfillDays = 30
	// msPerMinute converts milliseconds to minutes.
	msPerMinute = 60000
)

// fixesPattern matches "Fixes #123", "Closes #456", "Resolves #789" in PR bodies.
var fixesPattern = regexp.MustCompile(`(?i)(?:fixes|closes|resolves)\s+#(\d+)`)

// ComputeMTTR fetches recently merged PRs from the primary repo, extracts
// "Fixes #N" references, looks up each issue's creation time, and computes
// issue-to-merge duration statistics.
func (c *Client) ComputeMTTR(ctx context.Context, primaryRepo string) (*MTTRResult, error) {
	owner, repo := c.splitRepo(primaryRepo)

	// List recently merged PRs
	opts := &gh.PullRequestListOptions{
		State:       "closed",
		Sort:        "updated",
		Direction:   "desc",
		ListOptions: gh.ListOptions{PerPage: mttrMergedPRLimit},
	}

	prs, _, err := c.client.PullRequests.List(ctx, owner, repo, opts)
	if err != nil {
		return nil, fmt.Errorf("listing merged PRs for %s/%s: %w", owner, repo, err)
	}

	// Extract issue references from merged PRs
	type issueRef struct {
		issueNum int
		mergedAt time.Time
	}
	var refs []issueRef

	for _, pr := range prs {
		if pr.GetMergedAt().IsZero() {
			continue // skip PRs that were closed without merging
		}
		body := pr.GetBody()
		if body == "" {
			continue
		}
		mergedAt := pr.GetMergedAt().Time
		matches := fixesPattern.FindAllStringSubmatch(body, -1)
		for _, m := range matches {
			var num int
			if _, err := fmt.Sscanf(m[1], "%d", &num); err == nil && num > 0 {
				refs = append(refs, issueRef{issueNum: num, mergedAt: mergedAt})
			}
		}
	}

	if len(refs) == 0 {
		return &MTTRResult{
			UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			History:   []MTTRBucket{},
		}, nil
	}

	// Deduplicate issue numbers for lookup
	issueNums := make(map[int]struct{})
	for _, ref := range refs {
		issueNums[ref.issueNum] = struct{}{}
	}

	// Fetch creation times for referenced issues
	createdAt := make(map[int]time.Time)
	for num := range issueNums {
		issue, _, err := c.client.Issues.Get(ctx, owner, repo, num)
		if err != nil {
			c.logger.Warn("failed to fetch issue for MTTR", "issue", num, "error", err)
			continue
		}
		createdAt[num] = issue.GetCreatedAt().Time
	}

	// Compute durations
	var durations []int // in minutes
	bucketMS := int64(mttrBucketHours) * 60 * msPerMinute
	bucketed := make(map[int64][]int) // bucket timestamp -> durations

	for _, ref := range refs {
		ca, ok := createdAt[ref.issueNum]
		if !ok {
			continue
		}
		issueMS := ca.UnixMilli()
		mergeMS := ref.mergedAt.UnixMilli()
		if mergeMS <= issueMS {
			continue
		}
		minutes := int((mergeMS - issueMS) / msPerMinute)
		durations = append(durations, minutes)
		bk := (mergeMS / bucketMS) * bucketMS
		bucketed[bk] = append(bucketed[bk], minutes)
	}

	if len(durations) == 0 {
		return &MTTRResult{
			UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			History:   []MTTRBucket{},
		}, nil
	}

	sort.Ints(durations)
	sum := 0
	for _, d := range durations {
		sum += d
	}
	avg := sum / len(durations)
	median := durations[len(durations)/2]
	p90Idx := int(float64(len(durations)) * 0.9)
	if p90Idx >= len(durations) {
		p90Idx = len(durations) - 1
	}
	p90 := durations[p90Idx]

	// Build history buckets (last N days only)
	cutoffMS := time.Now().Add(-mttrBackfillDays * 24 * time.Hour).UnixMilli()
	bucketKeys := make([]int64, 0, len(bucketed))
	for k := range bucketed {
		bucketKeys = append(bucketKeys, k)
	}
	sort.Slice(bucketKeys, func(i, j int) bool { return bucketKeys[i] < bucketKeys[j] })

	var history []MTTRBucket
	for _, t := range bucketKeys {
		if t < cutoffMS {
			continue
		}
		vals := bucketed[t]
		sort.Ints(vals)
		bSum := 0
		for _, v := range vals {
			bSum += v
		}
		history = append(history, MTTRBucket{
			T:      t,
			Avg:    bSum / len(vals),
			Median: vals[len(vals)/2],
		})
	}

	return &MTTRResult{
		AvgMinutes:     avg,
		MedianMinutes:  median,
		P90Minutes:     p90,
		Count:          len(durations),
		FastestMinutes: durations[0],
		SlowestMinutes: durations[len(durations)-1],
		UpdatedAt:      time.Now().UTC().Format(time.RFC3339),
		History:        history,
	}, nil
}
