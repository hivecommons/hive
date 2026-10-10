// Package eventdispatch turns GitHub App pull_request webhooks into early
// review dispatch (hivecommons/hive#11091).
//
// A webhook does not dispatch a reviewer by itself. It queues the PR here;
// once the PR has been quiet for the debounce window the dispatcher wakes the
// governor, which runs its normal eval cycle with that PR first in line. All
// the gates the cadence path applies (head age, loop caps, ACMM, hold labels,
// the per-head perspective budget) therefore apply unchanged, and the cadence
// tick keeps working as the fallback for hives without webhooks.
package eventdispatch

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultDebounce is how long a PR must go without another push before
	// its dispatch fires, so a burst of pushes is reviewed once.
	DefaultDebounce = 90 * time.Second
	// DefaultMinInterval spaces governor wakes, so many PRs pushed at once
	// cost one early eval cycle, not one each.
	DefaultMinInterval = 30 * time.Second
	// DefaultMaxPending bounds the queue; deliveries past it are dropped and
	// the cadence tick picks those PRs up.
	DefaultMaxPending = 256
	// DefaultMaxRecent bounds the recent-dispatch list served to the dashboard.
	DefaultMaxRecent = 50
	// DefaultDispatchedTTL is how long a fired (PR, head SHA) is remembered
	// for idempotency.
	DefaultDispatchedTTL = 24 * time.Hour
	// DefaultMaxDispatched bounds the idempotency memory.
	DefaultMaxDispatched = 4096
	// maxWaitFactor caps the trailing-edge debounce: a PR pushed continuously
	// still fires maxWaitFactor × debounce after its first event.
	maxWaitFactor = 4
)

// Trigger values reported on review pipeline cards.
const (
	TriggerEvent        = "event"
	TriggerEventPending = "event_pending"
	TriggerCadence      = "cadence"
)

// Enqueue outcomes.
const (
	OutcomeQueued    = "queued"
	OutcomeCoalesced = "coalesced"
	OutcomeDuplicate = "duplicate"
	OutcomeDropped   = "dropped"
	OutcomeIgnored   = "ignored"
)

// Request is one webhook delivery that asks for a review.
type Request struct {
	Repo    string
	Number  int
	HeadSHA string
	Event   string
	Action  string
}

// Entry is a pending or fired dispatch.
type Entry struct {
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	HeadSHA   string    `json:"head_sha,omitempty"`
	Event     string    `json:"event"`
	Action    string    `json:"action,omitempty"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	DueAt     time.Time `json:"due_at"`
	// Coalesced counts deliveries folded into this entry after the first.
	Coalesced int       `json:"coalesced"`
	FiredAt   time.Time `json:"fired_at,omitzero"`
	deadline  time.Time
}

// Stats are lifetime counters since process start.
type Stats struct {
	Received   int `json:"received"`
	Queued     int `json:"queued"`
	Coalesced  int `json:"coalesced"`
	Duplicates int `json:"duplicates"`
	Dropped    int `json:"dropped"`
	Fired      int `json:"fired"`
	Wakes      int `json:"wakes"`
}

// Status is the dispatcher snapshot served by GET /api/review/dispatch/events.
type Status struct {
	Pending    []Entry   `json:"pending"`
	Recent     []Entry   `json:"recent"`
	Stats      Stats     `json:"stats"`
	MaxPending int       `json:"max_pending"`
	LastWake   time.Time `json:"last_wake,omitzero"`
}

// Options configure a Dispatcher. Zero values take the defaults.
type Options struct {
	MinInterval   time.Duration
	MaxPending    int
	MaxRecent     int
	DispatchedTTL time.Duration
	MaxDispatched int
	// Now and After replace the clock and timer in tests.
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time
}

// Dispatcher is the bounded, debounced, idempotent event queue. It is safe
// for concurrent use; build one with New.
type Dispatcher struct {
	mu          sync.Mutex
	now         func() time.Time
	after       func(time.Duration) <-chan time.Time
	minInterval time.Duration
	maxPending  int
	maxRecent   int
	ttl         time.Duration
	maxFired    int
	pending     map[string]*Entry
	fired       map[string]time.Time
	recent      []Entry
	stats       Stats
	lastWake    time.Time
	changed     chan struct{}
}

// New builds a Dispatcher.
func New(opts Options) *Dispatcher {
	d := &Dispatcher{
		now:         opts.Now,
		after:       opts.After,
		minInterval: opts.MinInterval,
		maxPending:  opts.MaxPending,
		maxRecent:   opts.MaxRecent,
		ttl:         opts.DispatchedTTL,
		maxFired:    opts.MaxDispatched,
		pending:     map[string]*Entry{},
		fired:       map[string]time.Time{},
		changed:     make(chan struct{}, 1),
	}
	if d.now == nil {
		d.now = time.Now
	}
	if d.after == nil {
		d.after = time.After
	}
	if d.minInterval <= 0 {
		d.minInterval = DefaultMinInterval
	}
	if d.maxPending <= 0 {
		d.maxPending = DefaultMaxPending
	}
	if d.maxRecent <= 0 {
		d.maxRecent = DefaultMaxRecent
	}
	if d.ttl <= 0 {
		d.ttl = DefaultDispatchedTTL
	}
	if d.maxFired <= 0 {
		d.maxFired = DefaultMaxDispatched
	}
	return d
}

// repoName is the comparison key for a repo: the lower-cased name without the
// owner, because the governor's actionable list may carry bare repo names
// while webhooks carry owner/name.
func repoName(repo string) string {
	repo = strings.ToLower(strings.TrimSpace(repo))
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		return repo[i+1:]
	}
	return repo
}

func prKey(repo string, number int) string {
	return repoName(repo) + "#" + strconv.Itoa(number)
}

func headKey(repo string, number int, sha string) string {
	return prKey(repo, number) + "@" + strings.ToLower(strings.TrimSpace(sha))
}

// Enqueue queues req, debounced by debounce (DefaultDebounce when <= 0).
// A later delivery for the same PR replaces the head SHA and restarts the
// debounce, so a push burst fires once for its final head. A head that has
// already fired is not queued again.
func (d *Dispatcher) Enqueue(req Request, debounce time.Duration) string {
	if d == nil || strings.TrimSpace(req.Repo) == "" || req.Number <= 0 {
		return OutcomeIgnored
	}
	if debounce <= 0 {
		debounce = DefaultDebounce
	}
	req.HeadSHA = strings.TrimSpace(req.HeadSHA)
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	d.stats.Received++
	d.pruneFiredLocked(now)
	if req.HeadSHA != "" {
		if _, ok := d.fired[headKey(req.Repo, req.Number, req.HeadSHA)]; ok {
			d.stats.Duplicates++
			return OutcomeDuplicate
		}
	}
	key := prKey(req.Repo, req.Number)
	if e, ok := d.pending[key]; ok {
		e.Coalesced++
		e.LastSeen = now
		e.Event, e.Action = req.Event, req.Action
		if req.HeadSHA != "" {
			e.HeadSHA = req.HeadSHA
		}
		e.DueAt = minTime(now.Add(debounce), e.deadline)
		d.stats.Coalesced++
		d.signalLocked()
		return OutcomeCoalesced
	}
	if len(d.pending) >= d.maxPending {
		d.stats.Dropped++
		return OutcomeDropped
	}
	d.pending[key] = &Entry{
		Repo:      strings.TrimSpace(req.Repo),
		Number:    req.Number,
		HeadSHA:   req.HeadSHA,
		Event:     req.Event,
		Action:    req.Action,
		FirstSeen: now,
		LastSeen:  now,
		DueAt:     now.Add(debounce),
		deadline:  now.Add(maxWaitFactor * debounce),
	}
	d.stats.Queued++
	d.signalLocked()
	return OutcomeQueued
}

func (d *Dispatcher) signalLocked() {
	select {
	case d.changed <- struct{}{}:
	default:
	}
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// NextDue is when the next wake may happen: the earliest pending DueAt, held
// back to MinInterval after the previous wake. ok is false with nothing
// pending.
func (d *Dispatcher) NextDue() (time.Time, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.nextDueLocked()
}

func (d *Dispatcher) nextDueLocked() (time.Time, bool) {
	var next time.Time
	for _, e := range d.pending {
		if next.IsZero() || e.DueAt.Before(next) {
			next = e.DueAt
		}
	}
	if next.IsZero() {
		return time.Time{}, false
	}
	if !d.lastWake.IsZero() {
		if gate := d.lastWake.Add(d.minInterval); gate.After(next) {
			next = gate
		}
	}
	return next, true
}

// TakeDue removes and returns every entry due now, oldest first, records each
// fired head for idempotency and counts one wake. It returns nil while
// nothing is due or the previous wake was under MinInterval ago.
func (d *Dispatcher) TakeDue() []Entry {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	if !d.lastWake.IsZero() && now.Sub(d.lastWake) < d.minInterval {
		return nil
	}
	var out []Entry
	for key, e := range d.pending {
		if e.DueAt.After(now) {
			continue
		}
		delete(d.pending, key)
		e.FiredAt = now
		out = append(out, *e)
		if e.HeadSHA != "" {
			d.fired[headKey(e.Repo, e.Number, e.HeadSHA)] = now
		}
	}
	if len(out) == 0 {
		return nil
	}
	sortEntries(out, func(e Entry) time.Time { return e.FirstSeen })
	d.lastWake = now
	d.stats.Wakes++
	d.stats.Fired += len(out)
	d.recent = append(d.recent, out...)
	if over := len(d.recent) - d.maxRecent; over > 0 {
		d.recent = append(d.recent[:0], d.recent[over:]...)
	}
	d.capFiredLocked()
	return out
}

func sortEntries(es []Entry, at func(Entry) time.Time) {
	sort.Slice(es, func(i, j int) bool {
		a, b := at(es[i]), at(es[j])
		if !a.Equal(b) {
			return a.Before(b)
		}
		return prKey(es[i].Repo, es[i].Number) < prKey(es[j].Repo, es[j].Number)
	})
}

func (d *Dispatcher) pruneFiredLocked(now time.Time) {
	for k, at := range d.fired {
		if now.Sub(at) >= d.ttl {
			delete(d.fired, k)
		}
	}
}

func (d *Dispatcher) capFiredLocked() {
	over := len(d.fired) - d.maxFired
	if over <= 0 {
		return
	}
	keys := make([]string, 0, len(d.fired))
	for k := range d.fired {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := d.fired[keys[i]], d.fired[keys[j]]
		if !a.Equal(b) {
			return a.Before(b)
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys[:over] {
		delete(d.fired, k)
	}
}

// Run waits for due entries and hands each batch to wake until ctx ends.
// wake must not block for long; the governor wiring does a non-blocking send.
func (d *Dispatcher) Run(ctx context.Context, wake func([]Entry)) {
	if d == nil || wake == nil {
		return
	}
	for {
		var timer <-chan time.Time
		d.mu.Lock()
		next, ok := d.nextDueLocked()
		now := d.now()
		d.mu.Unlock()
		if ok {
			timer = d.after(max(next.Sub(now), 0))
		}
		select {
		case <-ctx.Done():
			return
		case <-d.changed:
		case <-timer:
			if batch := d.TakeDue(); len(batch) > 0 {
				wake(batch)
			}
		}
	}
}

// Trigger reports how the PR's current head reached review: TriggerEvent when
// a webhook dispatch fired for it, TriggerEventPending while one is queued,
// "" otherwise. repo may be bare or owner/name.
func (d *Dispatcher) Trigger(repo string, number int, headSHA string) string {
	if d == nil {
		return ""
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.pending[prKey(repo, number)]; ok {
		return TriggerEventPending
	}
	if strings.TrimSpace(headSHA) != "" {
		if _, ok := d.fired[headKey(repo, number, headSHA)]; ok {
			return TriggerEvent
		}
	}
	return ""
}

// Snapshot returns the pending queue (soonest first), the recent fired
// dispatches (newest first) and the counters.
func (d *Dispatcher) Snapshot() Status {
	if d == nil {
		return Status{Pending: []Entry{}, Recent: []Entry{}}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	st := Status{
		Pending:    make([]Entry, 0, len(d.pending)),
		Recent:     make([]Entry, 0, len(d.recent)),
		Stats:      d.stats,
		MaxPending: d.maxPending,
		LastWake:   d.lastWake,
	}
	for _, e := range d.pending {
		st.Pending = append(st.Pending, *e)
	}
	sortEntries(st.Pending, func(e Entry) time.Time { return e.DueAt })
	for i := len(d.recent) - 1; i >= 0; i-- {
		st.Recent = append(st.Recent, d.recent[i])
	}
	return st
}

// RequestFromWebhook maps a GitHub webhook to a review request. ok is false
// for events and actions that do not ask for a review: only pull_request
// opened, reopened, synchronize, ready_for_review and review_requested on an
// open, non-draft PR qualify.
func RequestFromWebhook(event, action, repo string, number int, headSHA, state string, draft bool) (Request, bool) {
	if event != "pull_request" {
		return Request{}, false
	}
	switch action {
	case "opened", "reopened", "synchronize", "ready_for_review", "review_requested":
	default:
		return Request{}, false
	}
	if draft || (state != "" && state != "open") || strings.TrimSpace(repo) == "" || number <= 0 {
		return Request{}, false
	}
	return Request{Repo: repo, Number: number, HeadSHA: headSHA, Event: event, Action: action}, true
}
