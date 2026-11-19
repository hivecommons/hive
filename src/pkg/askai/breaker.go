package askai

import (
	"sync"
	"time"
)

const (
	// DefaultBreakerFailures consecutive upstream failures open the breaker.
	DefaultBreakerFailures = 3
	// DefaultBreakerCooldown is how long an open breaker short-circuits to
	// the docs link before one trial call is let through.
	DefaultBreakerCooldown = 5 * time.Minute
)

// breakerEvent reports a state change for the audit line.
type breakerEvent int

const (
	breakerNoChange breakerEvent = iota
	breakerOpened
	breakerClosed
)

// breaker is a consecutive-failure circuit breaker with a half-open trial.
type breaker struct {
	mu        sync.Mutex
	threshold int
	cooldown  time.Duration
	failures  int
	openUntil time.Time
	open      bool
	trial     bool
}

func newBreaker(threshold int, cooldown time.Duration) *breaker {
	if threshold <= 0 {
		threshold = DefaultBreakerFailures
	}
	if cooldown <= 0 {
		cooldown = DefaultBreakerCooldown
	}
	return &breaker{threshold: threshold, cooldown: cooldown}
}

// allow reports whether a call may go out, and how long to wait if not.
// After the cool-down exactly one trial call is admitted at a time.
func (b *breaker) allow(now time.Time) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.open {
		return true, 0
	}
	if now.Before(b.openUntil) {
		return false, b.openUntil.Sub(now)
	}
	if b.trial {
		return false, time.Second
	}
	b.trial = true
	return true, 0
}

// cancelTrial returns an admitted trial slot that never reached the vendor
// (for example, the request was rate limited after allow).
func (b *breaker) cancelTrial() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.trial = false
}

func (b *breaker) success() breakerEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	wasOpen := b.open
	b.failures, b.open, b.trial = 0, false, false
	if wasOpen {
		return breakerClosed
	}
	return breakerNoChange
}

func (b *breaker) failure(now time.Time) breakerEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	if b.open {
		// A failed trial re-opens for another cool-down.
		b.openUntil, b.trial = now.Add(b.cooldown), false
		return breakerNoChange
	}
	if b.failures >= b.threshold {
		b.open, b.openUntil, b.trial = true, now.Add(b.cooldown), false
		return breakerOpened
	}
	return breakerNoChange
}

func (b *breaker) isOpen() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.open
}
