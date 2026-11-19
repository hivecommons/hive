package ask

import (
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Denial says which limit stopped a request.
type Denial string

const (
	DenyNone   Denial = ""
	DenyPerson Denial = "person"
	DenyHive   Denial = "hive"
	DenyGlobal Denial = "global"
	DenyBusy   Denial = "busy"
	DenyBudget Denial = "budget"
)

// Limiter enforces the Ask limits: per person (a one-minute burst and an hourly
// cap, one request in flight), per hive (hourly), hub-wide (per minute and in
// flight) and a daily budget. In memory only; a restart resets it, which
// matches "no per-person cost data is kept beyond the hourly counter in
// memory" in the design record.
type Limiter struct {
	mu  sync.Mutex
	now func() time.Time
	lim config.AskLimits

	person         map[string][]time.Time
	hive           map[string][]time.Time
	global         []time.Time
	inFlight       int
	personInFlight map[string]bool
	day            string
	dayCount       int
}

// NewLimiter builds a limiter. now may be nil for time.Now.
func NewLimiter(lim config.AskLimits, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{
		now: now, lim: lim,
		person: map[string][]time.Time{}, hive: map[string][]time.Time{},
		personInFlight: map[string]bool{},
	}
}

// prune drops timestamps at or before cutoff.
func prune(ts []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(ts) && !ts[i].After(cutoff) {
		i++
	}
	return ts[i:]
}

// retryAfter is how long until the oldest kept timestamp leaves its window.
func retryAfter(ts []time.Time, window time.Duration, now time.Time) time.Duration {
	if len(ts) == 0 {
		return time.Second
	}
	d := ts[0].Add(window).Sub(now)
	if d < time.Second {
		d = time.Second
	}
	return d
}

// Acquire reserves one request for person (and hive, which may be empty). On
// success it returns a release func the caller must call when the vendor call
// ends. On denial it returns the limit that stopped it and a retry hint; nothing
// is counted for a denied request.
func (l *Limiter) Acquire(person, hive string) (release func(), denial Denial, wait time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()

	day := now.UTC().Format("2006-01-02")
	if day != l.day {
		l.day, l.dayCount = day, 0
	}
	if l.dayCount >= l.lim.Daily {
		next := now.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
		return nil, DenyBudget, next.Sub(now)
	}

	l.global = prune(l.global, now.Add(-time.Minute))
	if len(l.global) >= l.lim.GlobalPerMinute {
		return nil, DenyGlobal, retryAfter(l.global, time.Minute, now)
	}
	if l.inFlight >= l.lim.InFlight || l.personInFlight[person] {
		return nil, DenyBusy, 2 * time.Second
	}

	pt := prune(l.person[person], now.Add(-time.Hour))
	l.person[person] = pt
	if len(pt) >= l.lim.PersonPerHour {
		return nil, DenyPerson, retryAfter(pt, time.Hour, now)
	}
	burst := 0
	for _, t := range pt {
		if t.After(now.Add(-time.Minute)) {
			burst++
		}
	}
	if burst >= l.lim.PersonBurst {
		for _, t := range pt {
			if t.After(now.Add(-time.Minute)) {
				return nil, DenyPerson, retryAfter([]time.Time{t}, time.Minute, now)
			}
		}
	}
	if hive != "" {
		ht := prune(l.hive[hive], now.Add(-time.Hour))
		l.hive[hive] = ht
		if len(ht) >= l.lim.HivePerHour {
			return nil, DenyHive, retryAfter(ht, time.Hour, now)
		}
		l.hive[hive] = append(ht, now)
	}

	l.person[person] = append(pt, now)
	l.global = append(l.global, now)
	l.dayCount++
	l.inFlight++
	l.personInFlight[person] = true
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.inFlight--
			delete(l.personInFlight, person)
		})
	}, DenyNone, 0
}

// DailyCount is how many requests were admitted today (UTC).
func (l *Limiter) DailyCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.now().UTC().Format("2006-01-02") != l.day {
		return 0
	}
	return l.dayCount
}

// Breaker is a circuit breaker for the vendor call. After threshold
// consecutive failures it opens for cooldown; then one probe is let through,
// and its result closes or re-opens it.
type Breaker struct {
	mu        sync.Mutex
	now       func() time.Time
	threshold int
	cooldown  time.Duration

	failures int
	openedAt time.Time
	open     bool
	probing  bool

	// OnOpen and OnClose are called (without the lock held) when the breaker
	// changes state, for the audit lines.
	OnOpen  func()
	OnClose func()
}

// NewBreaker builds a breaker. now may be nil for time.Now.
func NewBreaker(threshold int, cooldown time.Duration, now func() time.Time) *Breaker {
	if now == nil {
		now = time.Now
	}
	if threshold < 1 {
		threshold = 5
	}
	if cooldown <= 0 {
		cooldown = time.Minute
	}
	return &Breaker{now: now, threshold: threshold, cooldown: cooldown}
}

// Allow reports whether a vendor call may be made now. While open it refuses
// until the cool-down passes, then lets exactly one probe through.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.open {
		return true
	}
	if b.probing || b.now().Sub(b.openedAt) < b.cooldown {
		return false
	}
	b.probing = true
	return true
}

// Success records a good vendor call.
func (b *Breaker) Success() {
	b.mu.Lock()
	wasOpen := b.open
	b.failures, b.open, b.probing = 0, false, false
	cb := b.OnClose
	b.mu.Unlock()
	if wasOpen && cb != nil {
		cb()
	}
}

// Failure records a failed vendor call.
func (b *Breaker) Failure() {
	b.mu.Lock()
	b.failures++
	opened := false
	if b.probing || (!b.open && b.failures >= b.threshold) {
		opened = !b.open
		b.open, b.probing = true, false
		b.openedAt = b.now()
	}
	cb := b.OnOpen
	b.mu.Unlock()
	if opened && cb != nil {
		cb()
	}
}

// Open reports whether the breaker is currently refusing calls.
func (b *Breaker) Open() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.open
}
