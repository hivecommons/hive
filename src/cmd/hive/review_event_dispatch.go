package main

import (
	"sort"
	"sync/atomic"

	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/review/eventdispatch"
)

// activeReviewEvents is the boot's webhook review dispatcher, read by
// planReviewDispatch, which runs inside runEvalCycle with no *boot of its own.
var activeReviewEvents atomic.Pointer[eventdispatch.Dispatcher]

// installReviewEvents builds the webhook review dispatcher and its wake
// channel (hivecommons/hive#11091). The dashboard webhook receiver enqueues
// into it; runLoop drains its wakes.
func (b *boot) installReviewEvents() {
	if b.reviewEvents == nil {
		b.reviewEvents = eventdispatch.New(eventdispatch.Options{})
	}
	if b.reviewWake == nil {
		b.reviewWake = make(chan struct{}, 1)
	}
	activeReviewEvents.Store(b.reviewEvents)
}

// startReviewEvents runs the dispatcher until the boot context ends.
func (b *boot) startReviewEvents() {
	if b.reviewEvents == nil || b.reviewWake == nil || b.ctx == nil {
		return
	}
	go b.reviewEvents.Run(b.ctx, b.wakeForReviewEvents)
}

// wakeForReviewEvents asks the governor loop for an early eval cycle. The
// send never blocks: a wake already pending covers this batch too.
func (b *boot) wakeForReviewEvents(batch []eventdispatch.Entry) {
	if b.logger != nil {
		for _, e := range batch {
			b.logger.Info("review webhook dispatch due", "repo", e.Repo, "pr", e.Number,
				"head_sha", e.HeadSHA, "action", e.Action, "coalesced", e.Coalesced)
		}
	}
	select {
	case b.reviewWake <- struct{}{}:
	default:
	}
}

// prioritizeEventPRs moves PRs whose current head a review webhook fired for
// to the front, keeping order otherwise. PlanDispatch walks the list in order
// and still applies every gate, so this changes who is first, not who is
// eligible.
func prioritizeEventPRs(prs []review.PullRequest, d *eventdispatch.Dispatcher) []review.PullRequest {
	if d == nil || len(prs) < 2 {
		return prs
	}
	event := make([]bool, len(prs))
	found := false
	for i, pr := range prs {
		event[i] = d.Trigger(pr.Repo, pr.Number, pr.HeadSHA) == eventdispatch.TriggerEvent
		found = found || event[i]
	}
	if !found {
		return prs
	}
	idx := make([]int, len(prs))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(i, j int) bool { return event[idx[i]] && !event[idx[j]] })
	out := make([]review.PullRequest, len(prs))
	for i, k := range idx {
		out[i] = prs[k]
	}
	return out
}
