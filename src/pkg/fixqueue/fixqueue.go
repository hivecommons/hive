// Package fixqueue is the per-contributor red-PR fix scheduler behind
// stage/072-returned-to-contributor (hivecommons/hive#12436).
//
// The governing rule, in the operator's words: "contributors should clean up
// their mess before taking on new stuff". Every ordering decision the relay
// makes for a contributor — at a task boundary, on a dequeue, on a new-issue
// claim — asks this scheduler first. The scheduler only decides; it performs
// no I/O, so the dashboard hub (which owns the relay sockets) and the
// automerge stage engine (which owns the GitHub comments) stay the writers.
//
// Backpressure: a contributor with twenty PRs of which nineteen are red must
// not be interrupted beyond their advertised capacity for parallel work. At
// most Capacity.Slots() fix tasks are in flight per contributor; the rest wait
// in a per-contributor FIFO ordered by PR age (oldest first), with a human
// changes_requested outranking CI-red. Overflow that waits past its grace is
// handed back to the hive (Overflow) instead of interrupting the human.
package fixqueue

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Mode is how a dispatched fix reaches the contributor.
type Mode string

const (
	// ModeSpawn: the relay launches a parallel fix sub-session; the primary
	// session keeps its current task.
	ModeSpawn Mode = "spawn"
	// ModePreempt: the relay parks the primary task (claim kept alive) and
	// runs the fix in its place, resuming afterwards.
	ModePreempt Mode = "preempt"
	// ModeBoundary: handed out at a task boundary (the relay asked for its
	// next task) so nothing is interrupted.
	ModeBoundary Mode = "boundary"
	// ModeComment: no relay interrupt; the PR only carries the GitHub comment
	// (contributor offline/external, capacity exhausted, or focus window).
	ModeComment Mode = "comment"
)

// Cause is why a PR is red; two PRs with the same non-empty Cause fail for
// the same reason and are coalesced into one fix task.
type Cause struct {
	// Check is the failing check name ("" for review asks / conflicts).
	Check string
	// Signature is the error signature (first failing line, ratchet name,
	// "base-moved"...). Empty means the cause is unknown and never coalesces.
	Signature string
}

func (c Cause) coalescable() bool { return c.Check != "" && c.Signature != "" }

// Item is one red PR waiting for its author.
type Item struct {
	Repo     string
	Number   int
	HeadSHA  string
	Title    string
	URL      string
	OpenedAt time.Time
	Cause    Cause
	// HumanChangesRequested is true when a human reviewer asked for changes;
	// such PRs outrank CI-red ones in the queue.
	HumanChangesRequested bool
	// Summary is the diagnosis text shipped to the contributor.
	Summary string
	// EnqueuedAt is when the item entered the queue (set by Enqueue; read by
	// Overflow).
	EnqueuedAt time.Time
	// Attempts counts the dispatches of this item the relay reported failed
	// (set by Requeue); NotBefore is when an interrupt may retry it.
	Attempts  int
	NotBefore time.Time
}

// Key identifies an item.
func (it Item) Key() string { return Key(it.Repo, it.Number) }

// Key renders the canonical repo#number key.
func Key(repo string, number int) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(number)
}

// Capacity is what the contributor's relay advertised about parallel work.
type Capacity struct {
	// Parallel is true when the relay can run fix sub-sessions beside the
	// primary session (advertised `parallel_sessions`).
	Parallel bool
	// Max is the relay's max_parallel_fixes (sessions including the primary
	// when Parallel). Zero/unadvertised reads as 1.
	Max int
}

func (c Capacity) spawns() bool { return c.Parallel && c.Max >= 2 }

// Slots returns how many fix tasks may be in flight while a primary task
// runs: capacity−1 sub-sessions in spawn mode, one preemption otherwise.
func (c Capacity) Slots() int {
	if c.spawns() {
		return c.Max - 1
	}
	return 1
}

// BoundarySlots returns how many fix tasks may be in flight when the primary
// session is idle (task boundary): the whole capacity.
func (c Capacity) BoundarySlots() int {
	if c.Max < 1 {
		return 1
	}
	return c.Max
}

// Mode is the interrupt mode this capacity implies.
func (c Capacity) Mode() Mode {
	if c.spawns() {
		return ModeSpawn
	}
	return ModePreempt
}

// Dispatch is one fix task handed to a contributor: one or more PRs that
// share a cause.
type Dispatch struct {
	ID    string
	Mode  Mode
	Items []Item
}

// Status is the per-contributor view exposed on the profile API.
type Status struct {
	Queued   int      `json:"queued"`
	InFlight int      `json:"in_flight"`
	Slots    int      `json:"slots"`
	Mode     string   `json:"mode"`
	Pending  []string `json:"pending,omitempty"`
	// FocusUntil is set in preempt mode while the contributor's resumed
	// primary task is protected from a second interruption.
	FocusUntil *time.Time `json:"focus_until,omitempty"`
}

// Options tunes the scheduler.
type Options struct {
	// MinFocusWindow is the minimum uninterrupted time a preempted primary
	// task must run after resuming before it may be preempted again.
	MinFocusWindow time.Duration
	Now            func() time.Time
}

// DefaultMinFocusWindow is contribute.min_focus_window's default.
const DefaultMinFocusWindow = 30 * time.Minute

type dispatch struct {
	id    string
	mode  Mode
	items map[string]Item
}

type contributor struct {
	queue    []Item
	inflight []*dispatch
	// preempted is true while a preemption is in flight (preempt mode allows one).
	preempted bool
	// focusUntil protects the resumed primary after a preemption completes.
	focusUntil time.Time
	seq        int
}

// LogFunc receives one line per enqueue/dispatch/dequeue/overflow.
type LogFunc func(event, user string, d Dispatch, queued int)

// Scheduler is the per-contributor fix queue. Safe for concurrent use.
type Scheduler struct {
	mu    sync.Mutex
	opts  Options
	by    map[string]*contributor
	onLog LogFunc
}

// New returns an empty scheduler.
func New(opts Options) *Scheduler {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MinFocusWindow <= 0 {
		opts.MinFocusWindow = DefaultMinFocusWindow
	}
	return &Scheduler{opts: opts, by: map[string]*contributor{}}
}

// SetLogger installs the enqueue/dequeue hook.
func (s *Scheduler) SetLogger(fn LogFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onLog = fn
}

// SetMinFocusWindow updates the focus window (config reload); d <= 0 is ignored.
func (s *Scheduler) SetMinFocusWindow(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d > 0 {
		s.opts.MinFocusWindow = d
	}
}

func norm(user string) string { return strings.ToLower(strings.TrimSpace(user)) }

func (s *Scheduler) get(user string) *contributor {
	c := s.by[norm(user)]
	if c == nil {
		c = &contributor{}
		s.by[norm(user)] = c
	}
	return c
}

// Enqueue adds (or refreshes) a red PR for user. It returns true when the
// item was new. An item already queued or in flight is refreshed in place
// (new head, same fix session) and keeps its place.
func (s *Scheduler) Enqueue(user string, it Item) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.get(user)
	key := it.Key()
	for _, d := range c.inflight {
		if old, ok := d.items[key]; ok {
			it.EnqueuedAt, it.Attempts, it.NotBefore = old.EnqueuedAt, old.Attempts, old.NotBefore
			d.items[key] = it
			return false
		}
	}
	for i, q := range c.queue {
		if q.Key() == key {
			it.EnqueuedAt, it.Attempts, it.NotBefore = q.EnqueuedAt, q.Attempts, q.NotBefore
			c.queue[i] = it
			sortQueue(c.queue)
			return false
		}
	}
	it.EnqueuedAt = s.opts.Now()
	c.queue = append(c.queue, it)
	sortQueue(c.queue)
	s.log("enqueue", user, Dispatch{Items: []Item{it}}, len(c.queue))
	return true
}

// sortQueue orders human changes_requested first, then oldest PR first.
func sortQueue(q []Item) {
	sort.SliceStable(q, func(i, j int) bool {
		if q[i].HumanChangesRequested != q[j].HumanChangesRequested {
			return q[i].HumanChangesRequested
		}
		if !q[i].OpenedAt.Equal(q[j].OpenedAt) {
			return q[i].OpenedAt.Before(q[j].OpenedAt)
		}
		return q[i].Number < q[j].Number
	})
}

// Remove drops a PR from user's queue or in-flight set (it went green, was
// closed, or left stage 072). It returns true when a slot was freed.
func (s *Scheduler) Remove(user, repo string, number int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.by[norm(user)]
	if !ok {
		return false
	}
	key := Key(repo, number)
	for i, q := range c.queue {
		if q.Key() == key {
			c.queue = append(c.queue[:i], c.queue[i+1:]...)
			s.log("remove", user, Dispatch{Items: []Item{q}}, len(c.queue))
			return false
		}
	}
	for i, d := range c.inflight {
		if _, ok := d.items[key]; ok {
			delete(d.items, key)
			if len(d.items) == 0 {
				c.inflight = append(c.inflight[:i], c.inflight[i+1:]...)
				s.settle(user, c, d)
				return true
			}
			return false
		}
	}
	return false
}

// settle records a finished dispatch: a completed preemption starts the
// focus window for the resumed primary task.
func (s *Scheduler) settle(user string, c *contributor, d *dispatch) {
	if d.mode == ModePreempt {
		c.preempted = false
		c.focusUntil = s.opts.Now().Add(s.opts.MinFocusWindow)
	}
	s.log("dequeue", user, d.public(), len(c.queue))
}

// Complete marks the fix session covering repo#number done (the relay
// reported task_complete/failed). The whole coalesced dispatch is settled.
// It returns true when a slot was freed.
func (s *Scheduler) Complete(user, repo string, number int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.by[norm(user)]
	if !ok {
		return false
	}
	key := Key(repo, number)
	for i, d := range c.inflight {
		if _, ok := d.items[key]; ok {
			c.inflight = append(c.inflight[:i], c.inflight[i+1:]...)
			s.settle(user, c, d)
			return true
		}
	}
	return false
}

// CompleteDispatch settles a dispatch by id.
func (s *Scheduler) CompleteDispatch(user, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.by[norm(user)]
	if !ok {
		return false
	}
	for i, d := range c.inflight {
		if d.id == id {
			c.inflight = append(c.inflight[:i], c.inflight[i+1:]...)
			s.settle(user, c, d)
			return true
		}
	}
	return false
}

// Requeue settles a dispatch the relay reported failed and puts its items
// back in the queue (keeping their place by age) with Attempts incremented
// and NotBefore = now+backoff, so an interrupt does not hand the same fix
// straight back to a relay that just refused it. It returns the requeued
// items; nil when id is not in flight.
func (s *Scheduler) Requeue(user, id string, backoff time.Duration) []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.by[norm(user)]
	if !ok {
		return nil
	}
	for i, d := range c.inflight {
		if d.id != id {
			continue
		}
		c.inflight = append(c.inflight[:i], c.inflight[i+1:]...)
		if d.mode == ModePreempt {
			// The primary was never parked for long: no focus window.
			c.preempted = false
		}
		out := d.public().Items
		notBefore := s.opts.Now().Add(backoff)
		for j := range out {
			out[j].Attempts++
			out[j].NotBefore = notBefore
			c.queue = append(c.queue, out[j])
		}
		sortQueue(c.queue)
		s.log("requeue", user, Dispatch{ID: d.id, Mode: d.mode, Items: out}, len(c.queue))
		return out
	}
	return nil
}

// Dispatch tries to hand user the next fix while a primary task is running.
// It returns nil when nothing may be dispatched now: empty queue, all slots
// busy, a preemption still in flight, the resumed primary's focus window
// has not elapsed, or the queue head is backing off after a failed dispatch.
// The returned Dispatch's Mode is spawn or preempt.
func (s *Scheduler) Dispatch(user string, cap Capacity) *Dispatch {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.by[norm(user)]
	if !ok || len(c.queue) == 0 || len(c.inflight) >= cap.Slots() {
		return nil
	}
	if s.opts.Now().Before(c.queue[0].NotBefore) {
		return nil
	}
	mode := cap.Mode()
	if mode == ModePreempt && (c.preempted || s.opts.Now().Before(c.focusUntil)) {
		return nil
	}
	return s.pop(user, c, mode)
}

// DispatchAtBoundary hands user the next fix when their relay asked for its
// next task (nothing is running, so nothing is interrupted). Fixes are
// served before any new issue; preemption rules do not apply. nil when the
// queue is empty or the whole capacity is busy.
func (s *Scheduler) DispatchAtBoundary(user string, cap Capacity) *Dispatch {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.by[norm(user)]
	if !ok || len(c.queue) == 0 || len(c.inflight) >= cap.BoundarySlots() {
		return nil
	}
	return s.pop(user, c, ModeBoundary)
}

// pop takes the queue head plus every queued item sharing its cause.
func (s *Scheduler) pop(user string, c *contributor, mode Mode) *Dispatch {
	head := c.queue[0]
	d := &dispatch{mode: mode, items: map[string]Item{head.Key(): head}}
	rest := c.queue[:0:0]
	for _, q := range c.queue[1:] {
		if head.Cause.coalescable() && q.Cause == head.Cause {
			d.items[q.Key()] = q
			continue
		}
		rest = append(rest, q)
	}
	c.queue = rest
	c.seq++
	d.id = "fix-" + norm(user) + "-" + strconv.Itoa(c.seq)
	if mode == ModePreempt {
		c.preempted = true
	}
	c.inflight = append(c.inflight, d)
	out := d.public()
	s.log("dispatch", user, out, len(c.queue))
	return &out
}

func (d *dispatch) public() Dispatch {
	items := make([]Item, 0, len(d.items))
	for _, it := range d.items {
		items = append(items, it)
	}
	sortQueue(items)
	return Dispatch{ID: d.id, Mode: d.mode, Items: items}
}

// Overflow removes and returns user's queued items that have waited longer
// than grace: pressure relief, the hive adopts them (stage 074) instead of
// interrupting the contributor.
func (s *Scheduler) Overflow(user string, grace time.Duration) []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.by[norm(user)]
	if !ok {
		return nil
	}
	now := s.opts.Now()
	var out []Item
	rest := c.queue[:0:0]
	for _, q := range c.queue {
		if now.Sub(q.EnqueuedAt) >= grace {
			out = append(out, q)
			continue
		}
		rest = append(rest, q)
	}
	c.queue = rest
	if len(out) > 0 {
		s.log("overflow", user, Dispatch{Items: out}, len(c.queue))
	}
	return out
}

// Pending reports whether user has queued fixes (so new work must wait).
func (s *Scheduler) Pending(user string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.by[norm(user)]
	return ok && len(c.queue) > 0
}

// Status returns user's queue depth and in-flight count.
func (s *Scheduler) Status(user string, cap Capacity) Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{Slots: cap.Slots(), Mode: string(cap.Mode())}
	c, ok := s.by[norm(user)]
	if !ok {
		return st
	}
	st.Queued = len(c.queue)
	for _, q := range c.queue {
		st.Pending = append(st.Pending, q.Key())
	}
	for _, d := range c.inflight {
		st.InFlight += len(d.items)
	}
	if s.opts.Now().Before(c.focusUntil) {
		fu := c.focusUntil
		st.FocusUntil = &fu
	}
	return st
}

// Queued returns user's queued items in dispatch order.
func (s *Scheduler) Queued(user string) []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.by[norm(user)]
	if !ok {
		return nil
	}
	return append([]Item(nil), c.queue...)
}

// InFlight returns the dispatches currently running for user.
func (s *Scheduler) InFlight(user string) []Dispatch {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.by[norm(user)]
	if !ok {
		return nil
	}
	out := make([]Dispatch, 0, len(c.inflight))
	for _, d := range c.inflight {
		out = append(out, d.public())
	}
	return out
}

func (s *Scheduler) log(event, user string, d Dispatch, queued int) {
	if s.onLog != nil {
		s.onLog(event, user, d, queued)
	}
}
