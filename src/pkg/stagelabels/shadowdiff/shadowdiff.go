// Package shadowdiff compares the legacy stage derivation with the central
// stage engine's while the engine runs in shadow mode (ADR-0025, PR-A).
//
// It is deliberately tiny and pure: it knows the stage vocabulary
// (pkg/stagelabels) and nothing about lanes, buckets or the engine itself.
// Diff reports how two decisions for the same item disagree; Recorder
// accumulates those disagreements across a shadow period as counters keyed
// by (kind, legacy, engine) — the labels of the
// stage_shadow_mismatch_total metric — plus a bounded ring of examples the
// dashboard status strip can show. Each distinct mismatch is logged once.
package shadowdiff

import (
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Decision is the part of a stage derivation the shadow comparison looks at:
// the stage and the reason recorded with it. Both derivers must agree on
// both before the engine may be switched to enforce.
type Decision struct {
	Stage  stagelabels.Stage
	Reason string
}

// Mismatch kinds. KindStage is the one that matters for the rollout gate;
// KindReason is informational (a different explanation for the same stage).
const (
	KindStage  = "stage"
	KindReason = "reason"
)

// Mismatch is one way two decisions for the same item disagree. Legacy and
// Engine are the rendered values for the kind (stage strings or reasons).
type Mismatch struct {
	Kind   string
	Legacy string
	Engine string
}

// Diff reports how engine disagrees with legacy. The result is empty when
// the two decisions are identical; otherwise it holds at most one entry per
// kind, stage first.
func Diff(legacy, engine Decision) []Mismatch {
	var out []Mismatch
	if legacy.Stage != engine.Stage {
		out = append(out, Mismatch{Kind: KindStage, Legacy: legacy.Stage.String(), Engine: engine.Stage.String()})
	}
	if legacy.Reason != engine.Reason {
		out = append(out, Mismatch{Kind: KindReason, Legacy: legacy.Reason, Engine: engine.Reason})
	}
	return out
}

// Example is one recorded mismatch with the item it was seen on.
type Example struct {
	Key    string
	Kind   string
	Legacy Decision
	Engine Decision
	At     time.Time
}

// Counter is one (kind, legacy, engine) bucket of the mismatch metric.
type Counter struct {
	Kind   string
	Legacy string
	Engine string
	Count  uint64
}

// Snapshot is a point-in-time copy of a Recorder for metrics and status.
type Snapshot struct {
	// Items counts Record calls; Agreed those with no mismatch of any kind;
	// StageMismatches those whose stage differed (the rollout gate).
	Items           uint64
	Agreed          uint64
	StageMismatches uint64
	// Counters is sorted by kind, legacy, engine.
	Counters []Counter
	// Examples holds the most recent mismatches, oldest first, bounded by
	// the recorder's example limit.
	Examples []Example
}

// DefaultExampleLimit bounds the example ring when no option sets it.
const DefaultExampleLimit = 50

// Option configures a Recorder.
type Option func(*Recorder)

// WithLogger sets the logger that receives one line per distinct mismatch.
// nil keeps the recorder silent.
func WithLogger(l *slog.Logger) Option { return func(r *Recorder) { r.logger = l } }

// WithExampleLimit bounds the example ring; values below 1 keep the default.
func WithExampleLimit(n int) Option {
	return func(r *Recorder) {
		if n > 0 {
			r.limit = n
		}
	}
}

// WithNow sets the clock used to stamp examples (tests).
func WithNow(now func() time.Time) Option {
	return func(r *Recorder) {
		if now != nil {
			r.now = now
		}
	}
}

// Recorder accumulates shadow-mode mismatches. It is safe for concurrent use.
type Recorder struct {
	mu     sync.Mutex
	now    func() time.Time
	logger *slog.Logger
	limit  int

	items     uint64
	agreed    uint64
	stageMiss uint64
	counts    map[Counter]uint64
	logged    map[Counter]bool
	ring      []Example
	next      int
	full      bool
}

// NewRecorder returns an empty recorder.
func NewRecorder(opts ...Option) *Recorder {
	r := &Recorder{now: time.Now, limit: DefaultExampleLimit, counts: map[Counter]uint64{}, logged: map[Counter]bool{}}
	for _, o := range opts {
		o(r)
	}
	r.ring = make([]Example, r.limit)
	return r
}

// Record compares the two decisions for key, accumulates any mismatch and
// returns what Diff reported. The first occurrence of each distinct
// (kind, legacy, engine) is logged at warn level.
func (r *Recorder) Record(key string, legacy, engine Decision) []Mismatch {
	diffs := Diff(legacy, engine)
	if r == nil {
		return diffs
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items++
	if len(diffs) == 0 {
		r.agreed++
		return diffs
	}
	at := r.now()
	for _, d := range diffs {
		if d.Kind == KindStage {
			r.stageMiss++
		}
		bucket := Counter{Kind: d.Kind, Legacy: d.Legacy, Engine: d.Engine}
		r.counts[bucket]++
		r.ring[r.next] = Example{Key: key, Kind: d.Kind, Legacy: legacy, Engine: engine, At: at}
		r.next++
		if r.next == r.limit {
			r.next, r.full = 0, true
		}
		if r.logger != nil && !r.logged[bucket] {
			r.logged[bucket] = true
			r.logger.Warn("stage shadow: legacy and engine disagree", "kind", d.Kind, "key", key, "legacy", d.Legacy, "engine", d.Engine, "legacy_stage", legacy.Stage.String(), "engine_stage", engine.Stage.String())
		}
	}
	return diffs
}

// Snapshot copies the recorder's state. Counters are sorted so two
// snapshots of the same state render identically.
func (r *Recorder) Snapshot() Snapshot {
	if r == nil {
		return Snapshot{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Snapshot{Items: r.items, Agreed: r.agreed, StageMismatches: r.stageMiss}
	s.Counters = make([]Counter, 0, len(r.counts))
	for c, n := range r.counts {
		c.Count = n
		s.Counters = append(s.Counters, c)
	}
	sort.Slice(s.Counters, func(i, j int) bool {
		a, b := s.Counters[i], s.Counters[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Legacy != b.Legacy {
			return a.Legacy < b.Legacy
		}
		return a.Engine < b.Engine
	})
	if r.full {
		s.Examples = append(s.Examples, r.ring[r.next:]...)
	}
	s.Examples = append(s.Examples, r.ring[:r.next]...)
	return s
}

// Reset clears every counter and example (a new shadow period).
func (r *Recorder) Reset() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items, r.agreed, r.stageMiss = 0, 0, 0
	r.counts = map[Counter]uint64{}
	r.logged = map[Counter]bool{}
	r.ring = make([]Example, r.limit)
	r.next, r.full = 0, false
}
