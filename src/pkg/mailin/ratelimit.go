package mailin

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Keyed limiter bookkeeping.
const (
	// limiterMaxKeys bounds the tracked keys; past it idle keys are pruned
	// and, if that is not enough, the table starts over.
	limiterMaxKeys = 10000
	// limiterIdle is how long an unused key is kept.
	limiterIdle = time.Hour
)

// RateLimiter is a token bucket per key (remote IP, sender domain).
type RateLimiter struct {
	mu    sync.Mutex
	every rate.Limit
	burst int
	now   func() time.Time
	m     map[string]*limEntry
}

type limEntry struct {
	lim  *rate.Limiter
	seen time.Time
}

// NewRateLimiter allows burst events at once and then n per period per
// key. n <= 0 disables the limit.
func NewRateLimiter(n int, period time.Duration, burst int, now func() time.Time) *RateLimiter {
	if now == nil {
		now = time.Now
	}
	l := &RateLimiter{burst: burst, now: now, m: map[string]*limEntry{}}
	if n > 0 && period > 0 {
		l.every = rate.Limit(float64(n) / period.Seconds())
	}
	if l.burst < 1 {
		l.burst = 1
	}
	return l
}

// Allow takes one token for key.
func (l *RateLimiter) Allow(key string) bool {
	if l == nil || l.every == 0 {
		return true
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.m[key]
	if !ok {
		if len(l.m) >= limiterMaxKeys {
			l.pruneLocked(now)
		}
		e = &limEntry{lim: rate.NewLimiter(l.every, l.burst)}
		l.m[key] = e
	}
	e.seen = now
	return e.lim.AllowN(now, 1)
}

func (l *RateLimiter) pruneLocked(now time.Time) {
	for k, e := range l.m {
		if now.Sub(e.seen) > limiterIdle {
			delete(l.m, k)
		}
	}
	if len(l.m) >= limiterMaxKeys {
		l.m = map[string]*limEntry{}
	}
}

// ByteBudget bounds the message bytes delivered per key (a hive's host)
// in a rolling window (hivecommons/hive#18564): a sender cannot fill one
// hive's spool by staying under the per-IP and per-domain message counts.
type ByteBudget struct {
	mu     sync.Mutex
	limit  int64
	window time.Duration
	now    func() time.Time
	m      map[string]*budgetEntry
}

type budgetEntry struct {
	start time.Time
	used  int64
}

// NewByteBudget allows limit bytes per key per window. limit <= 0
// disables the budget.
func NewByteBudget(limit int64, window time.Duration, now func() time.Time) *ByteBudget {
	if now == nil {
		now = time.Now
	}
	return &ByteBudget{limit: limit, window: window, now: now, m: map[string]*budgetEntry{}}
}

// Reserve takes n bytes of key's budget, false when they do not fit.
func (b *ByteBudget) Reserve(key string, n int64) bool {
	if b == nil || b.limit <= 0 {
		return true
	}
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.m[key]
	if !ok || now.Sub(e.start) >= b.window {
		if !ok && len(b.m) >= limiterMaxKeys {
			b.pruneLocked(now)
		}
		e = &budgetEntry{start: now}
		b.m[key] = e
	}
	if e.used+n > b.limit {
		return false
	}
	e.used += n
	return true
}

// Refund gives back n bytes Reserve took, for a delivery that failed.
func (b *ByteBudget) Refund(key string, n int64) {
	if b == nil || b.limit <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if e, ok := b.m[key]; ok {
		e.used = max(0, e.used-n)
	}
}

func (b *ByteBudget) pruneLocked(now time.Time) {
	for k, e := range b.m {
		if now.Sub(e.start) >= b.window {
			delete(b.m, k)
		}
	}
	if len(b.m) >= limiterMaxKeys {
		b.m = map[string]*budgetEntry{}
	}
}
