package governor

import (
	"sort"
	"strings"
	"time"
)

// FixerQueueStatus describes a PR awaiting delivery of its fix kick. Ahead is
// a pointer so the first queued PR emits zero while an unqueued PR omits it.
type FixerQueueStatus struct {
	FixerQueuePosition int  `json:"fixer_queue_position,omitempty"`
	FixerQueueAhead    *int `json:"fixer_queue_ahead,omitempty"`
}

type deferredFixKick struct {
	ref             string
	firstDeferredAt time.Time
}

// RecordFixKickDeferred preserves the first deferral across retries, new heads
// and fixer reassignment. This is visibility bookkeeping, not a dispatch gate.
func (g *Governor) RecordFixKickDeferred(ref string, at time.Time) {
	ref = strings.ToLower(strings.TrimSpace(ref))
	if g == nil || ref == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, kick := range g.deferredFixKicks {
		if kick.ref == ref {
			return
		}
	}
	g.deferredFixKicks = append(g.deferredFixKicks, deferredFixKick{ref: ref, firstDeferredAt: at})
	// Stable sorting keeps dispatch order when several deferrals share a clock
	// tick. Later retries never reset the timestamp or move an entry back.
	sort.SliceStable(g.deferredFixKicks, func(i, j int) bool {
		return g.deferredFixKicks[i].firstDeferredAt.Before(g.deferredFixKicks[j].firstDeferredAt)
	})
}

// RecordFixKickDelivered removes a PR only after its fix kick actually lands.
func (g *Governor) RecordFixKickDelivered(ref string) {
	ref = strings.ToLower(strings.TrimSpace(ref))
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, kick := range g.deferredFixKicks {
		if kick.ref == ref {
			g.deferredFixKicks = append(g.deferredFixKicks[:i], g.deferredFixKicks[i+1:]...)
			return
		}
	}
}

// ReconcileFixerQueue drops PRs absent from keep: the enumeration's open-PR
// set (PRs closed or merged while their fixer was busy) or the current plan's
// fix-kick refs (PRs that no longer need a fix).
func (g *Governor) ReconcileFixerQueue(keep map[string]bool) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	kept := g.deferredFixKicks[:0]
	for _, kick := range g.deferredFixKicks {
		if keep[kick.ref] {
			kept = append(kept, kick)
		}
	}
	g.deferredFixKicks = kept
}

// FixerQueue returns an independent snapshot keyed by owner/repo#number.
// Entries are one-based and reflect first-deferral age across all fixer lanes.
// The queue is in memory: after restart, dispatch rebuilds it from busy kicks.
func (g *Governor) FixerQueue() map[string]FixerQueueStatus {
	if g == nil {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if len(g.deferredFixKicks) == 0 {
		return nil
	}
	out := make(map[string]FixerQueueStatus, len(g.deferredFixKicks))
	for i, kick := range g.deferredFixKicks {
		ahead := i
		out[kick.ref] = FixerQueueStatus{FixerQueuePosition: i + 1, FixerQueueAhead: &ahead}
	}
	return out
}
