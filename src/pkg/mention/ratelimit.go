package mention

import (
	"sync"
	"time"
)

type RateLimiter struct {
	mu   sync.Mutex
	now  func() time.Time
	hits map[string][]time.Time
}

func NewRateLimiter(now func() time.Time) *RateLimiter {
	if now == nil {
		now = time.Now
	}
	return &RateLimiter{now: now, hits: map[string][]time.Time{}}
}

// Reserve takes one slot in key's sliding window of max hits per window. The
// returned release gives that slot back; callers invoke it when the attempt
// the slot paid for did not happen, so a retried attempt is charged once.
func (r *RateLimiter) Reserve(key string, max int, window time.Duration) (release func(), ok bool) {
	if max <= 0 {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	cutoff := now.Add(-window)
	kept := r.hits[key][:0]
	for _, t := range r.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= max {
		r.hits[key] = kept
		return nil, false
	}
	r.hits[key] = append(kept, now)
	return func() { r.refund(key, now) }, true
}

func (r *RateLimiter) refund(key string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	hits := r.hits[key]
	for i := len(hits) - 1; i >= 0; i-- {
		if hits[i].Equal(at) {
			r.hits[key] = append(hits[:i], hits[i+1:]...)
			return
		}
	}
}
