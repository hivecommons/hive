package github

import (
	"strings"
	"time"
)

// CIActivityClaimsMotion reports whether the snapshot still shows a check on
// its head executing or waiting: a check run whose status is not completed,
// or a commit status still pending — merge-gate contexts (Tide, DCO) aside,
// since those wait on review, not on CI.
func CIActivityClaimsMotion(a *PRCIActivity) bool {
	if a == nil {
		return false
	}
	for _, check := range a.Checks {
		if check.Head != "" && !strings.EqualFold(check.Head, a.Head) {
			continue
		}
		if IsMergeGateContext(check.Name) {
			continue
		}
		switch check.State {
		case "completed", "unknown", "":
			continue
		}
		return true
	}
	return false
}

// CIActivityStale reports whether a snapshot that still claims a check in
// motion was observed more than maxAge ago. GitHub's check runs finish in
// minutes; a snapshot that has said "running" for longer than that was not
// refreshed — the batch cache skipped a clean repository, the ETag replayed,
// or the listing was taken while the checks were starting
// (hivecommons/hive#17508: `CI 2/3 · running` eight hours after every check
// had passed). maxAge <= 0 disables the guard.
func CIActivityStale(a *PRCIActivity, now time.Time, maxAge time.Duration) bool {
	if a == nil || maxAge <= 0 || a.ObservedAt == nil || a.ObservedAt.IsZero() {
		return false
	}
	if !CIActivityClaimsMotion(a) {
		return false
	}
	return now.Sub(*a.ObservedAt) > maxAge
}

// RequestHeadCIRelist queues one coalesced check-run listing for the head,
// bypassing the GraphQL batch cache and any ETag: the result lands in the
// webhook CI evidence store, which HeadCIActivity lays over the cycle's
// listing on the next tracker build. A head relisted within minGap is left
// alone so a genuinely long-running check costs one listing per gap, not
// one per build. It reports whether a listing is now pending.
func (c *Client) RequestHeadCIRelist(repo string, number int, head, baseRef string, minGap time.Duration) bool {
	if c == nil || strings.TrimSpace(head) == "" || number <= 0 {
		return false
	}
	r := c.ciRefetcher()
	key := ciEvidenceKey(repo, head)
	r.mu.Lock()
	if r.relisted == nil {
		r.relisted = map[ciHeadKey]time.Time{}
	}
	now := sharedWebhookTracker().now()
	if last, ok := r.relisted[key]; ok && minGap > 0 && now.Sub(last) < minGap {
		r.mu.Unlock()
		return false
	}
	r.relisted[key] = now
	for k, at := range r.relisted {
		if now.Sub(at) > ciRelistMemory {
			delete(r.relisted, k)
		}
	}
	r.mu.Unlock()
	return r.enqueue(ciRefetchTarget{repo: repo, fullName: repo, head: head, number: number, baseRef: baseRef})
}

// ciRelistMemory bounds how long a head's last stale-relist time is kept.
const ciRelistMemory = 24 * time.Hour
