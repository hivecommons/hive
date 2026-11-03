package forgestate

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Bucket is a GitHub rate-limit bucket the Gate budgets separately.
type Bucket string

const (
	BucketCore   Bucket = "core"
	BucketSearch Bucket = "search"
)

// ErrBudgetExhausted is returned by a Gate that refuses a call.
var ErrBudgetExhausted = errors.New("forgestate: github api budget exhausted")

// Gate admits or refuses one GitHub call in a bucket. Every Fetcher call the
// Store makes passes it first; phase-2 readers that still need the search
// API consult the same Gate through Store.Admit so their spend shares one
// budget.
type Gate interface {
	Admit(ctx context.Context, b Bucket) error
}

// BudgetConfig configures NewBudgetGate.
type BudgetConfig struct {
	// PerMinute caps calls per bucket per Window. A missing or non-positive
	// entry leaves that bucket unlimited.
	PerMinute map[Bucket]int
	// Window is the accounting window; default one minute.
	Window time.Duration
	// Critical, when set, reports that the hive's GitHub API budget is in
	// critical mode (github.Client.APIBudgetMode). Search calls are refused
	// while it is true; core calls (reconcile, item re-fetch) still pass,
	// because they are what keeps the cache correct and 304 revalidations
	// are free against the primary quota.
	Critical func() bool
	Now      func() time.Time
	Metrics  Metrics
}

// BudgetGate is the default Gate: a fixed-window per-bucket call budget plus
// the hive API-budget critical hook. It complements, not replaces, the
// transport-level slow-start pacing the hive's GitHub client applies.
type BudgetGate struct {
	mu      sync.Mutex
	cfg     BudgetConfig
	started map[Bucket]time.Time
	used    map[Bucket]int
}

// NewBudgetGate returns a BudgetGate for c.
func NewBudgetGate(c BudgetConfig) *BudgetGate {
	if c.Window <= 0 {
		c.Window = time.Minute
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	limits := make(map[Bucket]int, len(c.PerMinute))
	for b, n := range c.PerMinute {
		limits[b] = n
	}
	c.PerMinute = limits
	return &BudgetGate{cfg: c, started: map[Bucket]time.Time{}, used: map[Bucket]int{}}
}

// Admit implements Gate.
func (g *BudgetGate) Admit(ctx context.Context, b Bucket) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if b == BucketSearch && g.cfg.Critical != nil && g.cfg.Critical() {
		return g.deny(b, "api budget critical")
	}
	g.mu.Lock()
	limit := g.cfg.PerMinute[b]
	if limit <= 0 {
		g.used[b]++
		g.mu.Unlock()
		return nil
	}
	now := g.cfg.Now()
	if start, ok := g.started[b]; !ok || now.Sub(start) >= g.cfg.Window || now.Before(start) {
		g.started[b] = now
		g.used[b] = 0
	}
	if g.used[b] >= limit {
		g.mu.Unlock()
		return g.deny(b, fmt.Sprintf("%d calls per %s used", limit, g.cfg.Window))
	}
	g.used[b]++
	g.mu.Unlock()
	return nil
}

// Used reports how many calls bucket b admitted in the current window (or in
// total for an unlimited bucket).
func (g *BudgetGate) Used(b Bucket) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.used[b]
}

func (g *BudgetGate) deny(b Bucket, why string) error {
	if g.cfg.Metrics != nil {
		g.cfg.Metrics.BudgetDenied(b)
	}
	return fmt.Errorf("%w: %s bucket: %s", ErrBudgetExhausted, b, why)
}
