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
func (r *RateLimiter) Allow(key string, max int, window time.Duration) bool {
	if max <= 0 {
		return false
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
		return false
	}
	r.hits[key] = append(kept, now)
	return true
}
