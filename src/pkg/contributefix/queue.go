// Package contributefix implements the per-contributor fix-before-new scheduler.
// It does not perform GitHub writes or interrupt a relay: the stage reconciler
// must authorize ownership, persist queue state, and acknowledge dispatch before
// using this policy. Each Queue belongs to ONE contributor, across their sessions.
package contributefix

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

const (
	Kind     = "fix-pr"
	Priority = "interrupt"
	Spawn    = "spawn"
	Preempt  = "preempt"
)

// Feedback is a head-specific snapshot, supplied by the trusted reconciler,
// never by the relay. Cause coalescing requires both Check and ErrorSignature;
// changes-requested feedback is never merged with unrelated review requests.
type Feedback struct {
	Repo             string    `json:"repo"`
	Number           int       `json:"number"`
	HeadSHA          string    `json:"head_sha"`
	CreatedAt        time.Time `json:"created_at"`
	ChangesRequested bool      `json:"changes_requested,omitempty"`
	FailingChecks    []string  `json:"failing_checks,omitempty"`
	ReviewAsks       []string  `json:"review_asks,omitempty"`
	Check            string    `json:"check,omitempty"`
	ErrorSignature   string    `json:"error_signature,omitempty"`
}

func (f Feedback) key() string { return fmt.Sprintf("%s#%d", f.Repo, f.Number) }

func sameCause(a, b Feedback) bool {
	return !a.ChangesRequested && !b.ChangesRequested && a.Repo == b.Repo && a.Check != "" && a.ErrorSignature != "" && a.Check == b.Check && a.ErrorSignature == b.ErrorSignature
}
func clone(f Feedback) Feedback {
	f.FailingChecks = append([]string(nil), f.FailingChecks...)
	f.ReviewAsks = append([]string(nil), f.ReviewAsks...)
	return f
}

// Task is the proposed relay payload. ID must be echoed on completion, so a
// delayed completion cannot release a newer assignment's capacity slot.
// Adapters must preserve the existing task-generation and credential gates.
type Task struct {
	ID          uint64     `json:"id"`
	Kind        string     `json:"kind"`
	Priority    string     `json:"priority"`
	Action      string     `json:"action"`
	PRs         []Feedback `json:"prs"`
	Instruction string     `json:"instruction"`
}

func cloneTask(t Task) Task {
	t.PRs = append([]Feedback(nil), t.PRs...)
	for i := range t.PRs {
		t.PRs[i] = clone(t.PRs[i])
	}
	return t
}

// Queue is safe for concurrent reconciler/relay callbacks. Its zero value is
// usable. It is an in-memory scheduling primitive, not a durable work ledger.
type Queue struct {
	mu             sync.Mutex
	pending        map[string]Feedback
	active         map[uint64]Task
	nextID         uint64
	awaitingResume bool
	focusUntil     time.Time
}

// Enqueue replaces queued feedback for a PR when its head or asks change.
// An unchanged in-flight head is idempotent; a newer head waits until the old
// assignment releases its slot. Ownership must be checked by the caller.
func (q *Queue) Enqueue(f Feedback) error {
	if f.Repo == "" || f.Number <= 0 || f.HeadSHA == "" || f.CreatedAt.IsZero() {
		return errors.New("contributefix: repo, PR number, head SHA and creation time are required")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, t := range q.active {
		for _, p := range t.PRs {
			if p.key() == f.key() && p.HeadSHA == f.HeadSHA {
				return nil
			}
		}
	}
	if q.pending == nil {
		q.pending = make(map[string]Feedback)
	}
	q.pending[f.key()] = clone(f)
	return nil
}

// Remove cancels only queued feedback for this exact head (green, closed, or
// adopted). A stale observation must not erase feedback for a new head. Active
// tasks still need an explicit Finish acknowledgement.
func (q *Queue) Remove(repo string, number int, head string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	key := (Feedback{Repo: repo, Number: number}).key()
	if f, ok := q.pending[key]; ok && f.HeadSHA == head {
		delete(q.pending, key)
	}
}

// Dispatch reserves available fix slots. parallelSessions counts TOTAL sessions,
// including the primary; omitted/zero means one. For parallel relays the primary
// is never preempted. maxParallelFixes is a server-side upper bound, not a grant
// of trust or an override of the contributor's advertised capacity.
// Offline contributors are left queued for the caller's grace/adoption policy.
func (q *Queue) Dispatch(now time.Time, online bool, parallelSessions, maxParallelFixes int) []Task {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !online {
		return nil
	}
	limit, action := parallelSessions-1, Spawn
	if parallelSessions <= 1 {
		limit, action = 1, Preempt
		if q.awaitingResume || now.Before(q.focusUntil) {
			return nil
		}
	}
	if maxParallelFixes < 1 {
		maxParallelFixes = 1
	}
	if limit > maxParallelFixes {
		limit = maxParallelFixes
	}
	slots := limit - len(q.active)
	if slots <= 0 {
		return nil
	}
	busy := make(map[string]bool)
	for _, t := range q.active {
		for _, f := range t.PRs {
			busy[f.key()] = true
		}
	}
	var ordered []Feedback
	for key, f := range q.pending {
		if busy[key] {
			continue
		}
		coalescingActive := false
		for _, t := range q.active {
			for _, p := range t.PRs {
				if sameCause(f, p) {
					coalescingActive = true
				}
			}
		}
		// Do not mutate a payload already delivered to the relay. New PRs with
		// that cause wait for its task to finish, then form the next batch.
		if !coalescingActive {
			ordered = append(ordered, f)
		}
	}
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.ChangesRequested != b.ChangesRequested {
			return a.ChangesRequested
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.key() < b.key()
	})
	var out []Task
	used := make(map[string]bool)
	for _, f := range ordered {
		if len(out) >= slots {
			break
		}
		if used[f.key()] {
			continue
		}
		q.nextID++
		t := Task{ID: q.nextID, Kind: Kind, Priority: Priority, Action: action,
			Instruction: "Fix the listed feedback. Push a new head to each existing PR branch with the same Signed-off-by identity; do not open replacement PRs or merge them.",
			PRs:         []Feedback{clone(f)}}
		used[f.key()] = true
		if !f.ChangesRequested && f.Check != "" && f.ErrorSignature != "" {
			for _, other := range ordered {
				if !used[other.key()] && sameCause(f, other) {
					t.PRs = append(t.PRs, clone(other))
					used[other.key()] = true
				}
			}
		}
		if q.active == nil {
			q.active = make(map[uint64]Task)
		}
		q.active[t.ID] = t
		for _, p := range t.PRs {
			delete(q.pending, p.key())
		}
		out = append(out, cloneTask(t))
		if action == Preempt {
			q.awaitingResume = true
			break
		}
	}
	return out
}

// Finish frees exactly one reservation. The adapter must dispatch again to
// refill parallel slots. Failure/retry policy belongs to the reconciler.
func (q *Queue) Finish(id uint64) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.active[id]; !ok {
		return false
	}
	delete(q.active, id)
	return true
}

// Reject rolls back a reservation whose delivery failed BEFORE the relay
// accepted it. It must not be used for an accepted task's failure: that task
// must Finish and, for preemption, acknowledge ResumePrimary instead.
func (q *Queue) Reject(id uint64) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	t, ok := q.active[id]
	if !ok {
		return false
	}
	if q.pending == nil {
		q.pending = make(map[string]Feedback)
	}
	for _, f := range t.PRs {
		if _, newer := q.pending[f.key()]; !newer {
			q.pending[f.key()] = clone(f)
		}
	}
	delete(q.active, id)
	if t.Action == Preempt {
		q.awaitingResume = false
	}
	return true
}

// ResumePrimary starts the focus window when the parked task actually resumes,
// not when its fix started or finished. It cannot be called while a preempting
// fix is still running. Duplicate resume acknowledgements do not extend focus.
func (q *Queue) ResumePrimary(now time.Time, minFocus time.Duration) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.awaitingResume {
		return false
	}
	for _, t := range q.active {
		if t.Action == Preempt {
			return false
		}
	}
	q.awaitingResume = false
	if minFocus < 0 {
		minFocus = 0
	}
	q.focusUntil = now.Add(minFocus)
	return true
}

// Counts reports queued PRs and in-flight TASKS (coalesced tasks can list
// multiple PRs). New work is allowed only when both are zero and the primary
// has been resumed. Adapters must check this at every new-work claim boundary.
func (q *Queue) Counts() (queuedPRs, inFlightTasks int, canClaimNew bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending), len(q.active), len(q.pending) == 0 && len(q.active) == 0 && !q.awaitingResume
}
