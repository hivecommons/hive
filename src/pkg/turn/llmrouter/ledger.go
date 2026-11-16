package llmrouter

import (
	"sync"
	"time"
)

// Ledger records per-provider availability: cooldowns after failures and
// requests spent against a daily cap. A stateless coordinator backs it with
// its own store so budgets survive between passes; the router itself keeps
// nothing.
type Ledger interface {
	// Acquire reserves one request for provider at now. It returns ok=false
	// and the earliest time the provider may be tried again when the
	// provider is cooling down or has spent dailyCap requests in the current
	// UTC day. dailyCap <= 0 means uncapped.
	Acquire(provider string, dailyCap int, now time.Time) (ok bool, retryAt time.Time, err error)
	// Cooldown marks provider unavailable until the given time. A cooldown
	// never shortens an existing longer one.
	Cooldown(provider string, until time.Time) error
}

// MemoryLedger is the in-process Ledger.
type MemoryLedger struct {
	mu    sync.Mutex
	state map[string]*ledgerEntry
}

type ledgerEntry struct {
	cooldownUntil time.Time
	day           string
	used          int
}

// NewMemoryLedger returns an empty in-memory ledger.
func NewMemoryLedger() *MemoryLedger { return &MemoryLedger{state: map[string]*ledgerEntry{}} }

func (l *MemoryLedger) entry(provider string) *ledgerEntry {
	e, ok := l.state[provider]
	if !ok {
		e = &ledgerEntry{}
		l.state[provider] = e
	}
	return e
}

// Acquire implements Ledger.
func (l *MemoryLedger) Acquire(provider string, dailyCap int, now time.Time) (bool, time.Time, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entry(provider)
	if now.Before(e.cooldownUntil) {
		return false, e.cooldownUntil, nil
	}
	day := now.UTC().Format(time.DateOnly)
	if e.day != day {
		e.day, e.used = day, 0
	}
	if dailyCap > 0 && e.used >= dailyCap {
		y, m, d := now.UTC().Date()
		return false, time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC), nil
	}
	e.used++
	return true, time.Time{}, nil
}

// Cooldown implements Ledger.
func (l *MemoryLedger) Cooldown(provider string, until time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entry(provider)
	if until.After(e.cooldownUntil) {
		e.cooldownUntil = until
	}
	return nil
}
