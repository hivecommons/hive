package github

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gh "github.com/google/go-github/v72/github"
)

const prDetailCacheMaxEntries = 4096

type prDetailCacheKey struct {
	repo   string
	number int
}

type prDetailCacheEntry struct {
	pr        *gh.PullRequest
	headSHA   string
	updatedAt time.Time
	fetchedAt time.Time
	lastUsed  time.Time
}

type prDetailCache struct {
	mu         sync.Mutex
	entries    map[prDetailCacheKey]*prDetailCacheEntry
	maxEntries int
	now        func() time.Time

	hits, misses atomic.Int64
}

func newPRDetailCache() *prDetailCache {
	return &prDetailCache{
		entries:    map[prDetailCacheKey]*prDetailCacheEntry{},
		maxEntries: prDetailCacheMaxEntries,
		now:        time.Now,
	}
}

var sharedPRDetailCache = newPRDetailCache()

func PRDetailCacheStats() (hits, misses, entries int64) {
	c := sharedPRDetailCache
	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	return c.hits.Load(), c.misses.Load(), int64(n)
}

func (c *prDetailCache) get(repo string, number int, headSHA string, updatedAt time.Time, ttl time.Duration) (*gh.PullRequest, bool) {
	return c.getLocked(repo, number, ttl, false, func(e *prDetailCacheEntry) bool {
		return strings.TrimSpace(headSHA) != "" && !updatedAt.IsZero() && e.headSHA == headSHA && e.updatedAt.Equal(updatedAt)
	})
}

func (c *prDetailCache) getAny(repo string, number int, ttl time.Duration) (*gh.PullRequest, bool) {
	return c.getLocked(repo, number, ttl, false, func(e *prDetailCacheEntry) bool { return true })
}

func (c *prDetailCache) getAnyAllowUnknown(repo string, number int, ttl time.Duration) (*gh.PullRequest, bool) {
	return c.getLocked(repo, number, ttl, true, func(e *prDetailCacheEntry) bool { return true })
}

func (c *prDetailCache) getMergedBy(repo string, number int) (*gh.PullRequest, bool) {
	if c == nil || number <= 0 {
		if c != nil {
			c.misses.Add(1)
		}
		return nil, false
	}
	key := prDetailCacheKey{repo: canonicalPRDetailRepo(repo), number: number}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || safeGetLogin(e.pr.GetMergedBy()) == "" {
		c.misses.Add(1)
		return nil, false
	}
	e.lastUsed = c.now()
	c.hits.Add(1)
	return clonePRDetail(e.pr), true
}

func (c *prDetailCache) getLocked(repo string, number int, ttl time.Duration, allowUnknownMergeable bool, match func(*prDetailCacheEntry) bool) (*gh.PullRequest, bool) {
	if c == nil || number <= 0 || ttl <= 0 {
		if c != nil {
			c.misses.Add(1)
		}
		return nil, false
	}
	key := prDetailCacheKey{repo: canonicalPRDetailRepo(repo), number: number}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		c.misses.Add(1)
		return nil, false
	}
	now := c.now()
	expired := now.Sub(e.fetchedAt) >= ttl && !sharedWebhookTracker.cleanAndHealthy(repo, number)
	if expired || !match(e) || (!allowUnknownMergeable && strings.EqualFold(strings.TrimSpace(e.pr.GetMergeableState()), "unknown")) {
		c.misses.Add(1)
		return nil, false
	}
	e.lastUsed = now
	c.hits.Add(1)
	return clonePRDetail(e.pr), true
}

func (c *prDetailCache) put(repo string, number int, pr *gh.PullRequest) {
	if c == nil || number <= 0 || pr == nil {
		return
	}
	key := prDetailCacheKey{repo: canonicalPRDetailRepo(repo), number: number}
	now := c.now()
	entry := &prDetailCacheEntry{
		pr:        clonePRDetail(pr),
		headSHA:   pr.GetHead().GetSHA(),
		updatedAt: pr.GetUpdatedAt().Time,
		fetchedAt: now,
		lastUsed:  now,
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = entry
	c.evictOldestLocked()
}

// invalidate drops the cached detail for (repo, number) so the next lookup
// re-fetches it. Used by webhook-driven invalidation (#11177).
func (c *prDetailCache) invalidate(repo string, number int) bool {
	if c == nil || number <= 0 {
		return false
	}
	key := prDetailCacheKey{repo: canonicalPRDetailRepo(repo), number: number}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.entries[key]
	delete(c.entries, key)
	return ok
}

// numbersMatching returns the PR numbers cached for any of repos whose head
// SHA, head ref or base ref satisfy match.
func (c *prDetailCache) numbersMatching(repos []string, match func(headSHA, headRef, baseRef string) bool) []int {
	if c == nil {
		return nil
	}
	want := make(map[string]bool, len(repos))
	for _, r := range repos {
		want[canonicalPRDetailRepo(r)] = true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []int
	for k, e := range c.entries {
		if !want[k.repo] {
			continue
		}
		if match(e.headSHA, e.pr.GetHead().GetRef(), e.pr.GetBase().GetRef()) {
			out = append(out, k.number)
		}
	}
	return out
}

func (c *prDetailCache) evictOldestLocked() {
	for len(c.entries) > c.maxEntries {
		var oldestKey prDetailCacheKey
		var oldest time.Time
		first := true
		for k, e := range c.entries {
			if first || e.lastUsed.Before(oldest) {
				first = false
				oldest = e.lastUsed
				oldestKey = k
			}
		}
		delete(c.entries, oldestKey)
	}
}

func canonicalPRDetailRepo(repo string) string {
	return strings.ToLower(strings.TrimSpace(repo))
}

func clonePRDetail(pr *gh.PullRequest) *gh.PullRequest {
	if pr == nil {
		return nil
	}
	out := &gh.PullRequest{
		Number:              clonePtr(pr.Number),
		State:               clonePtr(pr.State),
		Title:               clonePtr(pr.Title),
		Body:                clonePtr(pr.Body),
		UpdatedAt:           cloneTimestamp(pr.UpdatedAt),
		ClosedAt:            cloneTimestamp(pr.ClosedAt),
		MergedAt:            cloneTimestamp(pr.MergedAt),
		Labels:              cloneLabels(pr.Labels),
		User:                cloneUserLogin(pr.User),
		Draft:               clonePtr(pr.Draft),
		Merged:              clonePtr(pr.Merged),
		Mergeable:           clonePtr(pr.Mergeable),
		MergeableState:      clonePtr(pr.MergeableState),
		MergedBy:            cloneUserLogin(pr.MergedBy),
		MergeCommitSHA:      clonePtr(pr.MergeCommitSHA),
		MaintainerCanModify: clonePtr(pr.MaintainerCanModify),
		Head:                clonePRBranchSHA(pr.Head),
		Base:                clonePRBranchRef(pr.Base),
	}
	return out
}

func clonePRBranchSHA(in *gh.PullRequestBranch) *gh.PullRequestBranch {
	if in == nil {
		return nil
	}
	return &gh.PullRequestBranch{SHA: clonePtr(in.SHA), Ref: clonePtr(in.Ref)}
}

func clonePRBranchRef(in *gh.PullRequestBranch) *gh.PullRequestBranch {
	if in == nil {
		return nil
	}
	return &gh.PullRequestBranch{Ref: clonePtr(in.Ref), SHA: clonePtr(in.SHA), Repo: cloneRepository(in.Repo)}
}

func cloneUserLogin(in *gh.User) *gh.User {
	if in == nil {
		return nil
	}
	return &gh.User{Login: clonePtr(in.Login)}
}

func cloneLabels(in []*gh.Label) []*gh.Label {
	out := make([]*gh.Label, 0, len(in))
	for _, l := range in {
		if l == nil {
			continue
		}
		out = append(out, &gh.Label{Name: clonePtr(l.Name)})
	}
	return out
}

func cloneRepository(in *gh.Repository) *gh.Repository {
	if in == nil {
		return nil
	}
	return &gh.Repository{FullName: clonePtr(in.FullName)}
}

func cloneTimestamp(in *gh.Timestamp) *gh.Timestamp {
	if in == nil {
		return nil
	}
	cp := *in
	return &cp
}

func clonePtr[T any](in *T) *T {
	if in == nil {
		return nil
	}
	v := *in
	return &v
}

func resetPRDetailCacheForTest(now func() time.Time, maxEntries int) func() {
	old := sharedPRDetailCache
	c := newPRDetailCache()
	if now != nil {
		c.now = now
	}
	if maxEntries > 0 {
		c.maxEntries = maxEntries
	}
	sharedPRDetailCache = c
	return func() { sharedPRDetailCache = old }
}
