package askai

import (
	"sync"
	"time"
)

const (
	minuteWindow = time.Minute
	hourWindow   = time.Hour
	dayWindow    = 24 * time.Hour
	// personInFlight: one request in flight per person (design).
	personInFlight = 1
	// maxTrackedKeys bounds the per-person and per-hive counter maps; expired
	// windows are dropped first.
	maxTrackedKeys = 10000
)

// Limits are the effective request limits (config.AskLimits, copied so this
// package does not import config).
type Limits struct {
	PersonPerMinute int
	PersonPerHour   int
	HivePerHour     int
	GlobalPerMinute int
	InFlight        int
	Daily           int
}

// window is a fixed-window counter.
type window struct {
	start time.Time
	count int
}

func (w *window) roll(now time.Time, size time.Duration) {
	if w.start.IsZero() || !now.Before(w.start.Add(size)) {
		w.start, w.count = now, 0
	}
}

func (w *window) retryAfter(now time.Time, size time.Duration) time.Duration {
	d := w.start.Add(size).Sub(now)
	if d < time.Second {
		return time.Second
	}
	return d
}

type personState struct {
	minute, hour window
	inFlight     int
}

// limitDecision is the result of a reservation.
type limitDecision struct {
	ok         bool
	retryAfter time.Duration
	dailySpent bool
}

// limiter enforces every cap in one place, all-or-nothing: a refused request
// consumes nothing.
type limiter struct {
	mu        sync.Mutex
	limits    Limits
	persons   map[string]*personState
	hives     map[string]*window
	minute    window
	day       window
	inFlight  int
	backoffTo time.Time
}

func newLimiter(l Limits) *limiter {
	return &limiter{limits: l, persons: map[string]*personState{}, hives: map[string]*window{}}
}

// reserve takes one slot for person (and hive when non-empty). release must
// be called exactly once after an ok reservation.
func (l *limiter) reserve(now time.Time, person, hive string) limitDecision {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(now)
	if now.Before(l.backoffTo) {
		return limitDecision{retryAfter: l.backoffTo.Sub(now)}
	}
	p := l.persons[person]
	if p == nil {
		p = &personState{}
	}
	p.minute.roll(now, minuteWindow)
	p.hour.roll(now, hourWindow)
	l.minute.roll(now, minuteWindow)
	l.day.roll(now, dayWindow)
	var hw *window
	if hive != "" {
		hw = l.hives[hive]
		if hw == nil {
			hw = &window{}
		}
		hw.roll(now, hourWindow)
	}
	switch {
	case p.inFlight >= personInFlight:
		return limitDecision{retryAfter: time.Second}
	case p.minute.count >= l.limits.PersonPerMinute:
		return limitDecision{retryAfter: p.minute.retryAfter(now, minuteWindow)}
	case p.hour.count >= l.limits.PersonPerHour:
		return limitDecision{retryAfter: p.hour.retryAfter(now, hourWindow)}
	case hw != nil && hw.count >= l.limits.HivePerHour:
		return limitDecision{retryAfter: hw.retryAfter(now, hourWindow)}
	case l.day.count >= l.limits.Daily:
		return limitDecision{retryAfter: l.day.retryAfter(now, dayWindow), dailySpent: true}
	case l.minute.count >= l.limits.GlobalPerMinute:
		return limitDecision{retryAfter: l.minute.retryAfter(now, minuteWindow)}
	case l.inFlight >= l.limits.InFlight:
		return limitDecision{retryAfter: time.Second}
	}
	p.minute.count++
	p.hour.count++
	p.inFlight++
	l.persons[person] = p
	if hw != nil {
		hw.count++
		l.hives[hive] = hw
	}
	l.minute.count++
	l.day.count++
	l.inFlight++
	return limitDecision{ok: true}
}

func (l *limiter) release(person string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if p := l.persons[person]; p != nil && p.inFlight > 0 {
		p.inFlight--
	}
	if l.inFlight > 0 {
		l.inFlight--
	}
}

// backoff stops all calls until now+d, after a vendor 429.
func (l *limiter) backoff(now time.Time, d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t := now.Add(d); t.After(l.backoffTo) {
		l.backoffTo = t
	}
}

// dailyUsed reports today's count and the budget.
func (l *limiter) dailyUsed(now time.Time) (int, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.day.roll(now, dayWindow)
	return l.day.count, l.limits.Daily
}

// prune drops idle counters once the maps grow large. Caller holds mu.
func (l *limiter) prune(now time.Time) {
	if len(l.persons) >= maxTrackedKeys {
		for k, p := range l.persons {
			if p.inFlight == 0 && !now.Before(p.hour.start.Add(hourWindow)) {
				delete(l.persons, k)
			}
		}
	}
	if len(l.hives) >= maxTrackedKeys {
		for k, w := range l.hives {
			if !now.Before(w.start.Add(hourWindow)) {
				delete(l.hives, k)
			}
		}
	}
}
