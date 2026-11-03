package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Pacing priority while the slow-start caution window is engaged.
//
// Observed live (hosted hive, 2026-10-21): after a secondary-limit 403 the
// pacer serialized requests ~2s apart, but the high-frequency tickers (the
// 10s automerge sweep, the request watchers, advisory lookups, heartbeat
// collectors) kept enqueueing. The queue ran minutes deep, every caller then
// failed with "slot would miss request deadline" — including the governor's
// enumeration and the GitHub App installation-token mint — and the eval
// cycle never completed until the pod was restarted.
//
// The fix is a priority ladder at the one place every caller converges:
//
//   - PacingHigh (the governor's enumeration/reconcile) may claim any slot
//     and is bounded only by its own deadline;
//   - PacingNormal (default) may queue at most slowStartNormalMaxQueue ahead;
//   - PacingLow (periodic housekeeping) may queue at most
//     slowStartLowPriorityMaxQueue ahead and should skip its whole tick via
//     ShedLowPriorityTick when the queue is already deeper than its budget.
//
// A request refused this way fails fast with ErrPacingShed instead of taking
// a slot, so low-priority load is shed rather than queued and the governor's
// slot is never more than a bounded distance away. Installation-token mints
// do not go through the pacer at all (newJWTClient): the App-JWT endpoints
// are a separate rate-limit bucket and a failed mint takes every other
// request down with it.

// PacingClass is a request's priority against the slow-start pacer.
type PacingClass int

const (
	// PacingNormal is the default for requests that carry no class.
	PacingNormal PacingClass = iota
	// PacingHigh is the governor's enumeration/reconcile pass.
	PacingHigh
	// PacingLow is periodic housekeeping that can wait for the next tick.
	PacingLow
)

// String returns the class name used in logs and errors.
func (p PacingClass) String() string {
	switch p {
	case PacingHigh:
		return "high"
	case PacingLow:
		return "low"
	default:
		return "normal"
	}
}

const (
	// slowStartLowPriorityMaxQueue bounds how far ahead a low-priority
	// request may queue while cautious. Roughly two paced slots: a tick that
	// cannot start within that is shed and retried on its next tick.
	slowStartLowPriorityMaxQueue = 5 * time.Second
	// slowStartNormalMaxQueue bounds how far ahead a normal request may
	// queue while cautious, which is also the longest a high-priority request
	// can be made to wait for a slot.
	slowStartNormalMaxQueue = 30 * time.Second
	// pacingShedLogInterval bounds "shed" logs per caller name.
	pacingShedLogInterval = time.Minute
)

// ErrPacingShed is returned when a request is refused without taking a slot
// because the caution-window queue is already deeper than its class allows.
var ErrPacingShed = errors.New("github pacing: request shed while rate-limit caution window is engaged")

type pacingClassKey struct{}

// WithPacingClass tags every request made with ctx with class.
func WithPacingClass(ctx context.Context, class PacingClass) context.Context {
	return context.WithValue(ctx, pacingClassKey{}, class)
}

// PacingClassOf returns the class ctx carries, PacingNormal when none.
func PacingClassOf(ctx context.Context) PacingClass {
	if ctx == nil {
		return PacingNormal
	}
	class, _ := ctx.Value(pacingClassKey{}).(PacingClass)
	return class
}

// maxQueue returns how far ahead of now a request of this class may be
// slotted, or false when it is unbounded (deadline only).
func (p PacingClass) maxQueue() (time.Duration, bool) {
	switch p {
	case PacingHigh:
		return 0, false
	case PacingLow:
		return slowStartLowPriorityMaxQueue, true
	default:
		return slowStartNormalMaxQueue, true
	}
}

// queueETALocked is how long a request claiming a slot now would wait.
func (st *slowStartState) queueETALocked(now time.Time) time.Duration {
	slot, _ := st.claimSlotLocked(now)
	if eta := slot.Sub(now); eta > 0 {
		return eta
	}
	return 0
}

// queueETA reports the current pacing wait; zero when not cautious.
func (st *slowStartState) queueETA(now time.Time) time.Duration {
	st.mu.Lock()
	defer st.mu.Unlock()
	if !now.Before(st.cautiousUntil) {
		return 0
	}
	return st.queueETALocked(now)
}

// PacingQueueETA reports how long a request made now through the shared
// GitHub transport would wait for its pacing slot; zero outside the caution
// window.
func PacingQueueETA() time.Duration {
	return sharedSlowStartState.queueETA(time.Now())
}

var pacingShedLog struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// logPacingShedOnce logs one shed per name per pacingShedLogInterval.
func logPacingShedOnce(name string, now time.Time, attrs ...any) {
	pacingShedLog.mu.Lock()
	defer pacingShedLog.mu.Unlock()
	if pacingShedLog.last == nil {
		pacingShedLog.last = map[string]time.Time{}
	}
	if last, ok := pacingShedLog.last[name]; ok && now.Sub(last) < pacingShedLogInterval {
		return
	}
	pacingShedLog.last[name] = now
	slog.Warn("github pacing: low-priority work shed while rate-limit caution window is engaged", append([]any{"caller", name}, attrs...)...)
}

// ShedLowPriorityTick reports whether a periodic housekeeping tick named
// name should be skipped outright: the caution window is engaged and the
// pacing queue is already deeper than budget (the time the tick could spend
// on its requests) or than slowStartLowPriorityMaxQueue. A shed tick logs
// once per minute per name and retries on its next tick, so it never adds
// to the queue the governor's enumeration is waiting behind.
func ShedLowPriorityTick(name string, budget time.Duration) bool {
	now := time.Now()
	st := sharedSlowStartState
	st.mu.Lock()
	cautious := now.Before(st.cautiousUntil)
	var eta time.Duration
	if cautious {
		eta = st.queueETALocked(now)
	}
	until := st.cautiousUntil
	st.mu.Unlock()
	if !cautious {
		return false
	}
	limit := slowStartLowPriorityMaxQueue
	if budget > 0 && budget < limit {
		limit = budget
	}
	if eta <= limit {
		return false
	}
	logPacingShedOnce(name, now, "queue_eta", eta.Round(time.Millisecond), "budget", limit, "caution_until", until)
	return true
}

// pacingShedError builds the ErrPacingShed returned by the transport.
func pacingShedError(class PacingClass, eta, limit time.Duration) error {
	return fmt.Errorf("%w (class %s, queue %s ahead, limit %s)", ErrPacingShed, class, eta.Round(time.Millisecond), limit)
}

// resetPacingShedLogForTest clears the per-name shed log throttle.
func resetPacingShedLogForTest() {
	pacingShedLog.mu.Lock()
	pacingShedLog.last = nil
	pacingShedLog.mu.Unlock()
}
