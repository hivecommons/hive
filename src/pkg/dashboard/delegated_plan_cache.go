package dashboard

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The "Let hive decide" preview answered in 2–6 s on hive's own spoke
// (hivecommons/hive#15773): planDelegatedDecision gathers the PR live from
// GitHub — the PR, its files, the author's permission level, the head's
// checks, every competing PR, the comments and reviews, the prior
// decisions — and the SPA showed nothing until the plan landed. The cache
// below makes the click instant when the SPA prefetched the plan as the
// row's decision options came into view, and makes a click on a plan still
// being gathered join that gather instead of starting a second one.
//
// A plan is a preview, not a decision: the decide endpoint re-gathers, so
// a cached plan a few seconds stale can only ever mis-preview. The TTL is
// short on purpose; a decision on the item drops its plans at once.

// delegatedPlanCacheTTLDefault is how long a gathered plan answers clicks.
const delegatedPlanCacheTTLDefault = 90 * time.Second

// delegatedPlanCacheTTLEnv overrides the TTL (a Go duration; "0" disables
// the cache — every click gathers).
const delegatedPlanCacheTTLEnv = "HIVE_DASHBOARD_DECIDE_PLAN_CACHE_TTL"

// delegatedPlanCacheMaxEntries bounds the cache; the oldest entry leaves
// when it is full.
const delegatedPlanCacheMaxEntries = 256

// delegatedPlanCacheTTL reads the TTL, env override first.
func delegatedPlanCacheTTL() time.Duration {
	if raw := strings.TrimSpace(os.Getenv(delegatedPlanCacheTTLEnv)); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d >= 0 {
			return d
		}
	}
	return delegatedPlanCacheTTLDefault
}

// delegatedPlanEntry is one gathered (or gathering) plan. done closes when
// plan/err are set; at is when the gather finished.
type delegatedPlanEntry struct {
	done chan struct{}
	plan *delegatedDecisionPlan
	err  error
	at   time.Time
	dur  time.Duration
}

// delegatedPlanCache keys gathered plans by what shapes them (repo,
// number, type, head, the request's own words and the viewer's merger
// role) and single-flights concurrent gathers of the same key.
type delegatedPlanCache struct {
	mu      sync.Mutex
	entries map[string]*delegatedPlanEntry
	ttl     time.Duration
}

func newDelegatedPlanCache(ttl time.Duration) *delegatedPlanCache {
	return &delegatedPlanCache{entries: map[string]*delegatedPlanEntry{}, ttl: ttl}
}

// delegatedPlanCacheKey is the cache key for one plan request.
func delegatedPlanCacheKey(repoFull string, number int, itemType string, mergerRole bool, req delegatedDecisionRequest) string {
	parts := []string{repoFull, strconv.Itoa(number), itemType, strconv.FormatBool(mergerRole),
		strings.TrimSpace(req.Head), strings.TrimSpace(req.Reason), strings.TrimSpace(req.Caption), strings.TrimSpace(req.SentinelReason)}
	return strings.Join(parts, "\x00")
}

// delegatedPlanCacheItemPrefix is the key prefix every plan of one item shares.
func delegatedPlanCacheItemPrefix(repoFull string, number int) string {
	return repoFull + "\x00" + strconv.Itoa(number) + "\x00"
}

// lookup returns a live entry (finished within the TTL, or still being
// gathered) for key, or nil.
func (c *delegatedPlanCache) lookup(key string, now time.Time) *delegatedPlanEntry {
	if c == nil || c.ttl <= 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[key]
	if e == nil {
		return nil
	}
	select {
	case <-e.done:
		if now.Sub(e.at) > c.ttl {
			delete(c.entries, key)
			return nil
		}
	default:
	}
	return e
}

// start registers a gather for key and returns the entry to fill with
// finish. With the cache disabled the entry is not retained.
func (c *delegatedPlanCache) start(key string, now time.Time) *delegatedPlanEntry {
	e := &delegatedPlanEntry{done: make(chan struct{})}
	if c == nil || c.ttl <= 0 {
		return e
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evictLocked(now)
	c.entries[key] = e
	return e
}

// finish publishes the gather's result.
func (c *delegatedPlanCache) finish(key string, e *delegatedPlanEntry, plan *delegatedDecisionPlan, err error, now time.Time, dur time.Duration) {
	e.plan, e.err, e.at, e.dur = plan, err, now, dur
	close(e.done)
	if err == nil || c == nil || c.ttl <= 0 {
		return
	}
	// A failed gather is not worth a TTL: the next click retries.
	c.mu.Lock()
	if c.entries[key] == e {
		delete(c.entries, key)
	}
	c.mu.Unlock()
}

// invalidate drops every plan of one item (a decision was just recorded).
func (c *delegatedPlanCache) invalidate(repoFull string, number int) {
	if c == nil {
		return
	}
	prefix := delegatedPlanCacheItemPrefix(repoFull, number)
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if strings.HasPrefix(k, prefix) {
			delete(c.entries, k)
		}
	}
}

// evictLocked drops expired entries and, when still full, the oldest one.
func (c *delegatedPlanCache) evictLocked(now time.Time) {
	for k, e := range c.entries {
		select {
		case <-e.done:
			if now.Sub(e.at) > c.ttl {
				delete(c.entries, k)
			}
		default:
		}
	}
	for len(c.entries) >= delegatedPlanCacheMaxEntries {
		oldestKey, oldest := "", time.Time{}
		for k, e := range c.entries {
			select {
			case <-e.done:
				if oldestKey == "" || e.at.Before(oldest) {
					oldestKey, oldest = k, e.at
				}
			default:
			}
		}
		if oldestKey == "" {
			return
		}
		delete(c.entries, oldestKey)
	}
}

// delegatedPlanResult is what the handler reports: the plan, how long the
// gather took and whether the cache (or a gather already in flight)
// answered.
type delegatedPlanResult struct {
	plan   *delegatedDecisionPlan
	err    error
	dur    time.Duration
	source string // "gather", "cache" or "inflight"
}

// cachedPlanDelegatedDecision answers from the cache, joins a gather in
// flight, or gathers and publishes.
func (s *Server) cachedPlanDelegatedDecision(ctx context.Context, repoFull string, number int, itemType string, mergerRole bool, req delegatedDecisionRequest) delegatedPlanResult {
	c := s.delegatedPlans()
	key := delegatedPlanCacheKey(repoFull, number, itemType, mergerRole, req)
	now := time.Now()
	if e := c.lookup(key, now); e != nil {
		source := "cache"
		select {
		case <-e.done:
		default:
			source = "inflight"
		}
		select {
		case <-e.done:
			return delegatedPlanResult{plan: e.plan, err: e.err, dur: e.dur, source: source}
		case <-ctx.Done():
			return delegatedPlanResult{err: ctx.Err(), source: source}
		}
	}
	e := c.start(key, now)
	start := time.Now()
	plan, err := s.planDelegatedDecision(ctx, repoFull, number, itemType, mergerRole, req)
	dur := time.Since(start)
	c.finish(key, e, plan, err, time.Now(), dur)
	return delegatedPlanResult{plan: plan, err: err, dur: dur, source: "gather"}
}

// delegatedPlans is the server's plan cache, created on first use.
func (s *Server) delegatedPlans() *delegatedPlanCache {
	s.delegatedPlanOnce.Do(func() {
		if s.delegatedPlanCache == nil {
			s.delegatedPlanCache = newDelegatedPlanCache(delegatedPlanCacheTTL())
		}
	})
	return s.delegatedPlanCache
}
