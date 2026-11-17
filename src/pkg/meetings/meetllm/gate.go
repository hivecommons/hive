package meetllm

import (
	"context"
	"fmt"
	"sync"
)

// Per-meeting parallelism (hivecommons/hive#18590): the meeting persona
// answers several requests at once, so one meeting may have more than one
// model call in flight. Gate bounds how many and keeps the meeting's token
// budget honest while they run.
const (
	// DefaultPerMeetingInFlight is how many model calls one meeting may
	// have in flight when its gate is built with zero.
	DefaultPerMeetingInFlight = 3
	// defaultCallEstimate is the tokens a call is assumed to cost before
	// the meeting has seen one: a persona prompt with retrieval snippets
	// and a short JSON answer.
	defaultCallEstimate = 2000
)

// Gate is one meeting's in-process admission for model calls: at most N in
// flight, and a token reservation per call so concurrent calls cannot all
// pass the budget check on the same ledger total and overspend it. The
// ledger stays the single record of spend; the reservation only covers the
// calls whose usage is not written yet.
type Gate struct {
	sem chan struct{}

	mu       sync.Mutex
	reserved int
	// estimate is the largest call cost seen so far (defaultCallEstimate
	// until a call reports usage).
	estimate int
}

// NewGate builds a gate admitting n concurrent calls; n <= 0 means
// DefaultPerMeetingInFlight.
func NewGate(n int) *Gate {
	if n <= 0 {
		n = DefaultPerMeetingInFlight
	}
	return &Gate{sem: make(chan struct{}, n), estimate: defaultCallEstimate}
}

// Size is how many calls the gate admits at once.
func (g *Gate) Size() int { return cap(g.sem) }

// InFlight is how many calls hold the gate now.
func (g *Gate) InFlight() int { return len(g.sem) }

// enter waits for a free place until ctx ends.
func (g *Gate) enter(ctx context.Context) error {
	select {
	case g.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("meeting-llm: meeting's model calls all busy: %w", ctx.Err())
	}
}

func (g *Gate) leave() { <-g.sem }

// reserve admits one call against the budget: it refuses (ErrBudget) when
// the ledger total plus the calls still in flight already reach maxTokens,
// and otherwise holds this call's estimate until settle.
func (g *Gate) reserve(used, maxTokens int) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if maxTokens > 0 && used+g.reserved >= maxTokens {
		return 0, ErrBudget
	}
	est := g.estimate
	g.reserved += est
	return est, nil
}

// settle releases a call's reservation once its usage is in the ledger and
// learns its cost for the next estimate.
func (g *Gate) settle(held, spent int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reserved -= held
	if g.reserved < 0 {
		g.reserved = 0
	}
	if spent > g.estimate {
		g.estimate = spent
	}
}

// Reserved is the tokens held for calls in flight.
func (g *Gate) Reserved() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reserved
}
