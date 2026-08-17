// Package timeline captures issue→PR lifecycle events into an in-memory,
// bounded, concurrency-safe timeline that the dashboard can render as a
// lifecycle view (issue enumerated → classified → kicked → PR opened →
// merged/blocked).
//
// It is deliberately decoupled from the tracing package: the timeline never
// imports OTel or governor internals. Instead, whatever produces lifecycle
// signals (the governor eval loop, an OTel SpanProcessor adapter, tests) simply
// hands the Store an Event via the Recorder interface. A tiny FromSpan helper
// translates a span name + attribute map into an Event so a span-processor
// adapter elsewhere can stay a thin shim.
//
// The Store is a bounded ring buffer (see MaxEvents) guarded by an RWMutex, so
// memory is capped regardless of fleet volume and concurrent Record/Recent/
// ByIssue/FleetHealth calls are safe. Every accessor is nil-guarded and returns
// empty (never nil) slices so JSON serialization always yields arrays.
package timeline

import (
	"sort"
	"sync"
	"time"
)

// MaxEvents is the fixed capacity of a Store's ring buffer. Once the buffer is
// full, each new Record evicts the oldest event. It bounds memory independent
// of fleet volume; sized to keep a meaningful lifecycle window without
// unbounded growth.
const MaxEvents = 500

// DefaultFleetWindow is the look-back window FleetHealth uses when the caller
// passes a non-positive duration. It bounds "recent" derived counts to a
// rolling window rather than the entire retained ring.
const DefaultFleetWindow = 6 * time.Hour

// Kind enumerates the lifecycle stages an Event can represent. The set is
// closed and ordered along the issue→PR journey.
type Kind string

const (
	// KindEnumerated: the issue was discovered/enumerated by the governor.
	KindEnumerated Kind = "enumerated"
	// KindClassified: the issue was triaged/classified (label, difficulty).
	KindClassified Kind = "classified"
	// KindKicked: an agent was kicked to work the issue.
	KindKicked Kind = "kicked"
	// KindPROpened: a pull request was opened for the issue.
	KindPROpened Kind = "pr_opened"
	// KindMerged: the pull request merged (terminal, success).
	KindMerged Kind = "merged"
	// KindBlocked: the issue/PR is blocked (terminal-ish, needs attention).
	KindBlocked Kind = "blocked"
)

// Event is a single lifecycle datapoint. It is a plain value type with JSON
// tags so the dashboard DTOs can embed it directly.
type Event struct {
	// ID is a stable identifier for the event (may be empty; callers that need
	// dedupe or anchoring supply one).
	ID string `json:"id"`
	// IssueRef identifies the issue the event pertains to, e.g. "org/repo#123".
	IssueRef string `json:"issueRef"`
	// Kind is the lifecycle stage. See the Kind constants.
	Kind Kind `json:"kind"`
	// Agent is the agent associated with the event, when applicable.
	Agent string `json:"agent,omitempty"`
	// At is the event time in Unix milliseconds.
	At int64 `json:"at"`
	// Attrs carries free-form structured context (labels, PR number, reason).
	Attrs map[string]string `json:"attrs,omitempty"`
}

// Recorder is the narrow write side of the timeline. The governor / tracing
// path depends only on this interface, keeping the timeline decoupled and
// trivially mockable.
type Recorder interface {
	Record(Event)
}

// Store is a bounded, concurrency-safe ring buffer of lifecycle Events.
//
// The zero value is NOT ready for use — construct with NewStore. Internally it
// keeps a fixed-size slice and a write cursor; the total number of events ever
// recorded (seq) lets Recent return items in chronological order without
// copying on every write.
type Store struct {
	mu   sync.RWMutex
	buf  []Event
	next int   // index of the next write slot
	seq  int64 // total events ever recorded (monotonic)
	cap  int   // ring capacity (== len(buf))
}

// NewStore returns a Store whose ring buffer holds up to MaxEvents events.
func NewStore() *Store {
	return NewStoreWithCap(MaxEvents)
}

// NewStoreWithCap returns a Store with an explicit capacity. A non-positive
// capacity falls back to MaxEvents so a Store is never zero-capacity.
func NewStoreWithCap(capacity int) *Store {
	if capacity <= 0 {
		capacity = MaxEvents
	}
	return &Store{
		buf: make([]Event, capacity),
		cap: capacity,
	}
}

// Record appends an event, evicting the oldest once the ring is full. If At is
// unset (<= 0) it is stamped with the current time so downstream ordering and
// FleetHealth windows always have a timestamp. Safe for concurrent use.
func (s *Store) Record(e Event) {
	if s == nil {
		return
	}
	if e.At <= 0 {
		e.At = time.Now().UnixMilli()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf[s.next] = e
	s.next = (s.next + 1) % s.cap
	s.seq++
}

// size returns the number of live events currently retained. Caller holds at
// least a read lock.
func (s *Store) size() int {
	if s.seq < int64(s.cap) {
		return int(s.seq)
	}
	return s.cap
}

// chronological returns all live events oldest→newest. Caller holds at least a
// read lock. Always returns a non-nil slice.
func (s *Store) chronological() []Event {
	n := s.size()
	out := make([]Event, 0, n)
	if n == 0 {
		return out
	}
	// The oldest live event sits at (next - n) mod cap.
	start := ((s.next-n)%s.cap + s.cap) % s.cap
	for i := 0; i < n; i++ {
		out = append(out, s.buf[(start+i)%s.cap])
	}
	return out
}

// Recent returns up to n most-recent events, newest first. A non-positive n
// returns all retained events. Always returns a non-nil slice.
func (s *Store) Recent(n int) []Event {
	if s == nil {
		return []Event{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	chrono := s.chronological()
	// Reverse into newest-first.
	rev := make([]Event, 0, len(chrono))
	for i := len(chrono) - 1; i >= 0; i-- {
		rev = append(rev, chrono[i])
	}
	if n > 0 && n < len(rev) {
		rev = rev[:n]
	}
	return rev
}

// ByIssue returns every retained event for the given issue ref, newest first.
// An empty ref or unknown ref yields an empty (non-nil) slice.
func (s *Store) ByIssue(ref string) []Event {
	if s == nil || ref == "" {
		return []Event{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	chrono := s.chronological()
	out := make([]Event, 0)
	for i := len(chrono) - 1; i >= 0; i-- {
		if chrono[i].IssueRef == ref {
			out = append(out, chrono[i])
		}
	}
	return out
}

// FleetHealth is a derived roll-up of lifecycle activity over a rolling window.
type FleetHealth struct {
	// WindowMs is the look-back window used to derive the counts, in ms.
	WindowMs int64 `json:"windowMs"`
	// InFlight is the number of distinct issues that have been kicked (or
	// enumerated/classified/pr_opened) but not yet reached a terminal state
	// (merged/blocked) within the window.
	InFlight int `json:"inFlight"`
	// Merged is the number of distinct issues that merged within the window.
	Merged int `json:"merged"`
	// Blocked is the number of distinct issues that were blocked within the
	// window.
	Blocked int `json:"blocked"`
	// Events is the total number of events observed within the window.
	Events int `json:"events"`
}

// FleetHealth derives roll-up counts over the given window (measured back from
// now). A non-positive window uses DefaultFleetWindow. Counts are per distinct
// issue: an issue that merged is counted only in Merged, one that is blocked
// (and not merged) only in Blocked, and anything else with activity is
// InFlight. Safe for concurrent use.
func (s *Store) FleetHealth(window time.Duration) FleetHealth {
	if window <= 0 {
		window = DefaultFleetWindow
	}
	fh := FleetHealth{WindowMs: window.Milliseconds()}
	if s == nil {
		return fh
	}
	cutoff := time.Now().Add(-window).UnixMilli()

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Track terminal state per issue within the window.
	merged := map[string]bool{}
	blocked := map[string]bool{}
	active := map[string]bool{} // any non-terminal activity

	for _, e := range s.chronological() {
		if e.At < cutoff {
			continue
		}
		fh.Events++
		if e.IssueRef == "" {
			continue
		}
		switch e.Kind {
		case KindMerged:
			merged[e.IssueRef] = true
		case KindBlocked:
			blocked[e.IssueRef] = true
		default:
			active[e.IssueRef] = true
		}
	}

	for ref := range merged {
		fh.Merged++
		delete(active, ref)
		delete(blocked, ref)
	}
	for ref := range blocked {
		fh.Blocked++
		delete(active, ref)
	}
	fh.InFlight = len(active)
	return fh
}

// TimelineDTO is the JSON contract the dashboard consumes: the most-recent
// events plus the derived fleet health. Both fields are always non-nil in the
// serialized form (Events is an array, never null).
type TimelineDTO struct {
	Events []Event     `json:"events"`
	Fleet  FleetHealth `json:"fleet"`
}

// Snapshot builds a TimelineDTO with up to n recent events (newest first) and
// fleet health over the given window. Non-positive n / window fall back to
// "all events" / DefaultFleetWindow. Never returns nil slices.
func (s *Store) Snapshot(n int, window time.Duration) TimelineDTO {
	events := s.Recent(n)
	if events == nil {
		events = []Event{}
	}
	return TimelineDTO{
		Events: events,
		Fleet:  s.FleetHealth(window),
	}
}

// spanKindMap maps known span names to lifecycle Kinds for FromSpan. Keeping it
// here (rather than in tracing) preserves the decoupling: the tracing/governor
// side names spans however it likes; the timeline owns the translation.
var spanKindMap = map[string]Kind{
	"governor.enumerate":  KindEnumerated,
	"governor.classify":   KindClassified,
	"governor.kick":       KindKicked,
	"agent.kick":          KindKicked,
	"agent.kicked":        KindKicked,
	"pr.opened":           KindPROpened,
	"pr.open":             KindPROpened,
	"pr.merged":           KindMerged,
	"pr.merge":            KindMerged,
	"issue.blocked":       KindBlocked,
	"agent.blocked":       KindBlocked,
	"lifecycle.enumerate": KindEnumerated,
	"lifecycle.classify":  KindClassified,
	"lifecycle.kick":      KindKicked,
	"lifecycle.pr_opened": KindPROpened,
	"lifecycle.merged":    KindMerged,
	"lifecycle.blocked":   KindBlocked,
}

// Attribute keys FromSpan understands. Attrs unrecognized here are still copied
// verbatim into Event.Attrs.
const (
	attrIssueRef = "issue.ref"
	attrAgent    = "agent"
	attrEventID  = "event.id"
)

// FromSpan translates a span name + attribute map into a lifecycle Event. It
// returns (Event, false) when the span name does not correspond to a lifecycle
// stage, so a span-processor adapter can cheaply skip non-lifecycle spans.
//
// Recognized attributes (issue.ref, agent, event.id) populate the typed Event
// fields; all attributes are also preserved in Event.Attrs. A nil attrs map is
// tolerated.
func FromSpan(name string, attrs map[string]string) (Event, bool) {
	kind, ok := spanKindMap[name]
	if !ok {
		return Event{}, false
	}
	e := Event{Kind: kind}
	if len(attrs) > 0 {
		e.ID = attrs[attrEventID]
		e.IssueRef = attrs[attrIssueRef]
		e.Agent = attrs[attrAgent]
		// Copy the full attr set so nothing is lost.
		cp := make(map[string]string, len(attrs))
		for k, v := range attrs {
			cp[k] = v
		}
		e.Attrs = cp
	}
	return e, true
}

// KnownKinds returns the closed set of lifecycle Kinds, ordered along the
// issue→PR journey. Handy for UIs building a fixed lifecycle axis.
func KnownKinds() []Kind {
	return []Kind{
		KindEnumerated,
		KindClassified,
		KindKicked,
		KindPROpened,
		KindMerged,
		KindBlocked,
	}
}

// sortEventsChrono is a small helper used by tests/callers that assemble events
// from multiple sources and want a stable oldest→newest order. It sorts in
// place by At, then ID for determinism.
func sortEventsChrono(events []Event) {
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].At != events[j].At {
			return events[i].At < events[j].At
		}
		return events[i].ID < events[j].ID
	})
}
