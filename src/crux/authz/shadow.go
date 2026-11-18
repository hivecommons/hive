package authz

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// EnvShadow turns shadow mode on when set to a true value. Unset, empty or
	// anything unparseable is off. Read once per process by DefaultShadow.
	EnvShadow = "HIVE_AUTHZ_SHADOW"
	// EnvShadowBuffer overrides the number of events the stream keeps.
	EnvShadowBuffer = "HIVE_AUTHZ_SHADOW_BUFFER"

	// DefaultBufferEvents is the stream capacity when EnvShadowBuffer is unset
	// or invalid.
	DefaultBufferEvents = 1000
	// MaxBufferEvents caps EnvShadowBuffer so the stream stays small.
	MaxBufferEvents = 10000
	// maxLoggedDivergenceKinds bounds how many distinct divergence shapes are
	// logged. Every divergence is still counted; only the log line is limited,
	// so a hot mismatch cannot flood the log.
	maxLoggedDivergenceKinds = 256
)

// Sink receives each event after it is appended to the stream. It is optional
// and is the module boundary a later phase uses to hand events to the hive. An
// implementation must not block; a panic in it is recovered and counted and
// never reaches the caller.
type Sink interface {
	Record(Event)
}

// Stats is a point-in-time copy of a Shadow's counters.
type Stats struct {
	// Observed is the number of decisions recorded.
	Observed uint64
	// Mismatches is the number of decisions that differed from the existing gate.
	Mismatches uint64
	// Dropped is the number of events evicted from the bounded stream.
	Dropped uint64
	// Errors is the number of internal recording failures that were contained.
	Errors uint64
	// Buffered is how many events the stream holds now.
	Buffered int
	// Capacity is the stream's bound.
	Capacity int
}

// Shadow is the shadow-mode recorder. A nil *Shadow is the OFF state: every
// method is a safe no-op, nothing is recorded, nothing is allocated. Callers
// hold a possibly nil *Shadow and call Observe unconditionally.
type Shadow struct {
	// Logger receives divergence logs. Nil uses slog.Default().
	Logger *slog.Logger
	// Sink optionally receives every event.
	Sink Sink
	// Now supplies event timestamps. Nil uses time.Now.
	Now func() time.Time

	mu       sync.Mutex
	ring     []Event
	head     int
	count    int
	capacity int
	logged   map[string]struct{}

	observed   atomic.Uint64
	mismatches atomic.Uint64
	dropped    atomic.Uint64
	errors     atomic.Uint64
}

// NewShadow returns an enabled Shadow whose stream keeps at most capacity
// events. A non-positive capacity uses DefaultBufferEvents and anything above
// MaxBufferEvents is clamped.
func NewShadow(capacity int) *Shadow {
	if capacity <= 0 {
		capacity = DefaultBufferEvents
	}
	capacity = min(capacity, MaxBufferEvents)
	return &Shadow{
		ring:     make([]Event, capacity),
		capacity: capacity,
		logged:   make(map[string]struct{}),
	}
}

// ShadowFromEnv builds a Shadow from the environment, or returns nil (off)
// unless EnvShadow parses as true. Config-gated and default off.
func ShadowFromEnv(getenv func(string) string) *Shadow {
	if getenv == nil {
		return nil
	}
	on, err := strconv.ParseBool(strings.TrimSpace(getenv(EnvShadow)))
	if err != nil || !on {
		return nil
	}
	capacity := DefaultBufferEvents
	if raw := strings.TrimSpace(getenv(EnvShadowBuffer)); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			capacity = n
		}
	}
	return NewShadow(capacity)
}

var processShadow = sync.OnceValue(func() *Shadow { return ShadowFromEnv(os.Getenv) })

// DefaultShadow is the process-wide shadow built from the environment on first
// use. It is nil (off) unless EnvShadow is true, and a change to the
// environment after the first call has no effect until the process restarts.
func DefaultShadow() *Shadow {
	return processShadow()
}

// Observe evaluates req with Decide, compares the result with the verdict the
// existing gate already reached, records one event on the stream and logs a
// divergence. It enforces nothing and returns nothing: the existing gate's
// answer is the only one any caller acts on.
//
// It is safe on a nil receiver (off), safe for concurrent use, and contains
// its own failures: a panic anywhere in recording is recovered and counted,
// never propagated, so enabling the shadow cannot change what the caller does.
func (s *Shadow) Observe(req Request, legacy Verdict) {
	if s == nil {
		return
	}
	defer func() {
		if recover() != nil {
			s.errors.Add(1)
		}
	}()
	start := time.Now()
	d := Decide(req)
	d.Shadow = true
	latency := time.Since(start)

	ev := buildEvent(s.now(), req, d, legacy, latency)
	s.observed.Add(1)
	s.append(ev)
	if ev.Parity == ParityMismatch {
		s.mismatches.Add(1)
		s.logDivergence(ev)
	}
	s.deliver(ev)
}

// Events returns a copy of the stream, oldest first. Nil when off.
func (s *Shadow) Events() []Event {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.capacity == 0 {
		return nil
	}
	out := make([]Event, 0, s.count)
	start := (s.head - s.count + s.capacity) % s.capacity
	for i := range s.count {
		out = append(out, s.ring[(start+i)%s.capacity])
	}
	return out
}

// Stats returns the counters. The zero Stats when off.
func (s *Shadow) Stats() Stats {
	if s == nil {
		return Stats{}
	}
	s.mu.Lock()
	buffered := s.count
	s.mu.Unlock()
	return Stats{
		Observed:   s.observed.Load(),
		Mismatches: s.mismatches.Load(),
		Dropped:    s.dropped.Load(),
		Errors:     s.errors.Load(),
		Buffered:   buffered,
		Capacity:   s.capacity,
	}
}

func (s *Shadow) append(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.capacity == 0 {
		return
	}
	s.ring[s.head] = ev
	s.head = (s.head + 1) % s.capacity
	if s.count < s.capacity {
		s.count++
		return
	}
	s.dropped.Add(1)
}

func (s *Shadow) deliver(ev Event) {
	if s.Sink == nil {
		return
	}
	defer func() {
		if recover() != nil {
			s.errors.Add(1)
		}
	}()
	s.Sink.Record(ev)
}

func (s *Shadow) logDivergence(ev Event) {
	key := strings.Join([]string{ev.RuleID, ev.Effect, ev.Legacy.Effect, ev.Legacy.ReasonCode}, "|")
	s.mu.Lock()
	_, seen := s.logged[key]
	if !seen && len(s.logged) < maxLoggedDivergenceKinds {
		s.logged[key] = struct{}{}
	} else {
		seen = true
	}
	s.mu.Unlock()
	if seen {
		return
	}
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("authz shadow: decision differs from the existing gate; existing gate stays in force",
		"decision_id", ev.DecisionID,
		"surface", ev.Surface,
		"kind", ev.Action.Kind,
		"rule_id", ev.RuleID,
		"shadow_effect", ev.Effect,
		"shadow_reason", ev.ReasonCode,
		"legacy_effect", ev.Legacy.Effect,
		"legacy_reason", ev.Legacy.ReasonCode,
		"repo", ev.Resource.Repo,
		"branch", ev.Resource.Branch,
		"agent", ev.Actor.Agent,
		"policy_version", ev.PolicyVersion)
}

func (s *Shadow) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
