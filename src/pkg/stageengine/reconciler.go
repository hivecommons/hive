package stageengine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/stagelabels/shadowdiff"
)

// Transition is what Reconcile reports to its caller and the audit log.
type Transition struct {
	Ref      Ref
	From, To stagelabels.Stage
	Reason   string
	Anomaly  string // "" when expected; otherwise why the from→to pair is unexpected
	Outcome  stagelabels.Outcome
	// Err is the item's write error inside a ReconcileBatch; the batch
	// still reports the item so the caller can finish its side effects.
	Err error
}

// Reconciler derives, asserts the transition, records it in the Store and —
// in enforce mode — writes it. One instance per process.
type Reconciler interface {
	// Reconcile handles one item. In shadow mode it computes the Decision,
	// diffs it against legacy (via Mode/Shadow below) and writes nothing.
	Reconcile(ctx context.Context, f Facts) (Transition, error)
	// ReconcileBatch is what the sweeps call; it dedupes refs and preserves
	// per-item ordering with concurrent FactSink submissions.
	ReconcileBatch(ctx context.Context, facts []Facts) ([]Transition, error)
}

// Options configures New.
type Options struct {
	Mode   Mode
	Store  *stagelabels.Store // stagelabels.Shared() by default; API unchanged
	Writer StageWriter
	Issue  Deriver // defaults to DeriveIssue
	PR     Deriver // defaults to DerivePR
	// Legacy is the shadow-mode comparator: the sweep's own derivation
	// for f, written verbatim so a sweep routed through the reconciler
	// keeps today's labels. An Abstain means "the sweep had nothing to
	// write" and skips the item. Nil in enforce mode.
	Legacy  func(f Facts) Decision
	Now     func() time.Time
	Logger  *slog.Logger
	Metrics Metrics
	// Shadow is the PR-A recorder every shadow-mode comparison is fed:
	// it owns the (kind, legacy, engine) log-once and the bounded example
	// ring the dashboard status shows. nil keeps the reconciler's own
	// log-once.
	Shadow *shadowdiff.Recorder
}

// ErrNoWriter is returned by an enforce-mode Reconcile with no Writer.
var ErrNoWriter = errors.New("stage engine: enforce mode needs a StageWriter")

type reconciler struct {
	opts Options
	mode atomic.Pointer[Mode]

	locks sync.Map // Ref.Key() → *sync.Mutex

	mu        sync.Mutex
	anomalies map[string]bool // key+from→to already logged
	mismatch  map[string]bool // key+legacy→engine already logged
}

// New returns the Reconciler for o. Zero fields take their defaults: Mode
// DefaultMode, Store stagelabels.Shared(), Issue/PR the delegating
// DeriveIssue/DerivePR, Now time.Now, Logger slog.Default(), Metrics
// DefaultCounters().
func New(o Options) Reconciler {
	if o.Mode == "" {
		o.Mode = DefaultMode
	}
	if o.Store == nil {
		o.Store = stagelabels.Shared()
	}
	if o.Issue == nil {
		o.Issue = IssueDeriver()
	}
	if o.PR == nil {
		o.PR = PRDeriver()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Metrics == nil {
		o.Metrics = DefaultCounters()
	}
	r := &reconciler{opts: o, anomalies: map[string]bool{}, mismatch: map[string]bool{}}
	m := o.Mode
	r.mode.Store(&m)
	return r
}

// Mode implements ModeSwitch.
func (r *reconciler) Mode() Mode { return *r.mode.Load() }

// SetMode implements ModeSwitch. An invalid mode is ignored.
func (r *reconciler) SetMode(m Mode) bool {
	if !m.Valid() {
		return false
	}
	prev := r.mode.Swap(&m)
	if *prev == m {
		return false
	}
	r.opts.Logger.Warn("stage engine: mode changed", slog.String("from", prev.String()), slog.String("to", m.String()))
	return true
}

func (r *reconciler) lock(key string) func() {
	m, _ := r.locks.LoadOrStore(key, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (r *reconciler) deriver(k stagelabels.Item) Deriver {
	if k == stagelabels.PRItem {
		return r.opts.PR
	}
	return r.opts.Issue
}

// Reconcile implements Reconciler.
func (r *reconciler) Reconcile(ctx context.Context, f Facts) (Transition, error) {
	tr := Transition{Ref: f.Ref}
	mode := r.Mode()
	if mode == ModeOff {
		if r.opts.Legacy == nil || r.opts.Writer == nil {
			return tr, nil
		}
		return r.passThrough(ctx, f, tr)
	}
	if f.ObservedAt.IsZero() {
		f.ObservedAt = r.opts.Now()
	}
	unlock := r.lock(f.Ref.Key())
	defer unlock()

	d := r.deriver(f.Ref.Kind).Derive(f)
	shadowLegacy := mode == ModeShadow && r.opts.Legacy != nil
	if d.Abstain && !shadowLegacy {
		tr.Reason = d.Reason
		return tr, nil
	}
	if !d.Abstain {
		r.opts.Metrics.Derivation(f.Ref.Kind, d.Stage)
	}

	// What gets written: the engine's Decision in enforce mode; in shadow
	// mode the legacy result (when a comparator is configured), never the
	// engine's. The engine abstaining never blocks a legacy write: the
	// sweep decided, the engine's silence is the mismatch to record.
	want := d
	switch mode {
	case ModeShadow:
		if !shadowLegacy {
			return r.shadowOnly(tr, d), nil
		}
		legacy := r.opts.Legacy(f)
		if legacy.Abstain {
			tr.Reason = legacy.Reason
			return tr, nil
		}
		r.recordShadow(f, legacy, d)
		want = legacy
	case ModeEnforce:
		if r.opts.Writer == nil {
			return tr, ErrNoWriter
		}
	}

	key := f.Ref.Key()
	prev, _ := r.opts.Store.Get(key)
	tr.From, tr.To, tr.Reason = prev.Stage, want.Stage, want.Reason
	if a := Anomaly(f.Ref.Kind, tr.From, tr.To); a != "" {
		tr.Anomaly = a
		r.recordAnomaly(f.Ref, tr.From, tr.To, want.Reason, a)
	}
	r.opts.Store.Observe(key, want.Stage, want.Reason)
	noteCoveringPR(r.opts.Store, key, f, want.Stage)

	if r.opts.Writer == nil {
		return tr, nil
	}
	out, err := r.opts.Writer.Apply(ctx, f.Ref, f.Labels, want.Desired())
	tr.Outcome = out
	r.opts.Metrics.WriterWrite(f.Ref.Kind, OpAdd, len(out.Added))
	r.opts.Metrics.WriterWrite(f.Ref.Kind, OpRemove, len(out.Removed))
	if out.Healed {
		r.opts.Metrics.DuplicateHealed(f.Ref.Kind)
	}
	if err != nil {
		return tr, fmt.Errorf("stage engine: write %s: %w", f.Ref, err)
	}
	return tr, nil
}

// noteCoveringPR keeps the issue record's memory of its covering PR in
// step with the stage just observed: the named PR while the issue is at
// 040-covered-by-pr, nothing at any other stage. A 040 whose PR the facts
// did not name leaves whatever was remembered in place.
func noteCoveringPR(store *stagelabels.Store, key string, f Facts, stage stagelabels.Stage) {
	if store == nil || f.Issue == nil || f.Ref.Kind != stagelabels.IssueItem {
		return
	}
	switch {
	case stage != stagelabels.CoveredByPR:
		store.SetCoveringPR(key, "")
	case f.Issue.CoveringPR != nil && f.Issue.CoveringPR.Number > 0:
		store.SetCoveringPR(key, f.Issue.CoveringPR.Key())
	}
}

// shadowOnly is shadow mode without a comparator: the derivation is
// counted and reported against the Store's current record, nothing is
// observed or written.
func (r *reconciler) shadowOnly(tr Transition, d Decision) Transition {
	prev, _ := r.opts.Store.Get(tr.Ref.Key())
	tr.From, tr.To, tr.Reason = prev.Stage, d.Stage, d.Reason
	tr.Anomaly = Anomaly(tr.Ref.Kind, tr.From, tr.To)
	return tr
}

func (r *reconciler) recordAnomaly(ref Ref, from, to stagelabels.Stage, reason, anomaly string) {
	r.opts.Metrics.TransitionAnomaly(ref.Kind, from, to)
	k := ref.Key() + "|" + from.String() + "→" + to.String()
	r.mu.Lock()
	seen := r.anomalies[k]
	r.anomalies[k] = true
	r.mu.Unlock()
	if seen {
		return
	}
	r.opts.Logger.Warn(fmt.Sprintf("stage: unexpected %s→%s on %s: %s", from, to, ref, reason),
		slog.String("kind", ref.Kind.String()), slog.String("from", from.String()), slog.String("to", to.String()),
		slog.String("ref", ref.String()), slog.String("anomaly", anomaly))
}

// passThrough is ModeOff with a comparator: the sweep's own Decision is
// observed and written exactly as the pre-engine sweep did, with no
// derivation, comparison or counter. The rollback position.
func (r *reconciler) passThrough(ctx context.Context, f Facts, tr Transition) (Transition, error) {
	unlock := r.lock(f.Ref.Key())
	defer unlock()
	legacy := r.opts.Legacy(f)
	if legacy.Abstain {
		tr.Reason = legacy.Reason
		return tr, nil
	}
	key := f.Ref.Key()
	prev, _ := r.opts.Store.Get(key)
	tr.From, tr.To, tr.Reason = prev.Stage, legacy.Stage, legacy.Reason
	r.opts.Store.Observe(key, legacy.Stage, legacy.Reason)
	noteCoveringPR(r.opts.Store, key, f, legacy.Stage)
	out, err := r.opts.Writer.Apply(ctx, f.Ref, f.Labels, legacy.Desired())
	tr.Outcome = out
	if err != nil {
		return tr, fmt.Errorf("stage engine: write %s: %w", f.Ref, err)
	}
	return tr, nil
}

// recordShadow feeds one shadow comparison to the recorder (when
// configured) and, on a stage disagreement, to the mismatch counter.
func (r *reconciler) recordShadow(f Facts, legacy, d Decision) {
	if r.opts.Shadow != nil {
		engine := shadowdiff.Decision{Stage: d.Stage, Reason: d.Reason}
		if d.Abstain {
			engine.Reason = "abstain: " + d.Reason
		}
		r.opts.Shadow.Record(f.Ref.String(), shadowdiff.Decision{Stage: legacy.Stage, Reason: legacy.Reason}, engine)
	}
	if d.Abstain || legacy.Stage != d.Stage {
		r.recordMismatch(f, legacy.Stage, d)
	}
}

func (r *reconciler) recordMismatch(f Facts, legacy stagelabels.Stage, d Decision) {
	r.opts.Metrics.ShadowMismatch(f.Ref.Kind, legacy, d.Stage)
	if r.opts.Shadow != nil {
		return // the recorder logged the first occurrence of this bucket
	}
	k := f.Ref.Key() + "|" + legacy.String() + "→" + d.Stage.String()
	r.mu.Lock()
	seen := r.mismatch[k]
	r.mismatch[k] = true
	r.mu.Unlock()
	if seen {
		return
	}
	engine := d.Stage.String()
	if d.Abstain {
		engine = "abstain"
	}
	r.opts.Logger.Warn("stage engine: shadow mismatch",
		slog.String("kind", f.Ref.Kind.String()), slog.String("ref", f.Ref.String()),
		slog.String("legacy", legacy.String()), slog.String("engine", engine), slog.String("engine_reason", d.Reason))
}

// ReconcileBatch implements Reconciler. The last Facts for a Ref wins; the
// first occurrence fixes its position. Every item is attempted and every
// attempted item has a Transition in the result, in order, so a sweep can
// pair them with its own per-item work; an item whose write failed carries
// the error in Transition.Err and the first such error is also returned.
// A cancelled context stops the batch with the transitions so far.
func (r *reconciler) ReconcileBatch(ctx context.Context, facts []Facts) ([]Transition, error) {
	order := make([]string, 0, len(facts))
	latest := make(map[string]Facts, len(facts))
	for _, f := range facts {
		k := f.Ref.Key()
		if _, dup := latest[k]; !dup {
			order = append(order, k)
		}
		latest[k] = f
	}
	out := make([]Transition, 0, len(order))
	var firstErr error
	for _, k := range order {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		tr, err := r.Reconcile(ctx, latest[k])
		if err != nil {
			tr.Err = err
			if firstErr == nil {
				firstErr = err
			}
		}
		out = append(out, tr)
	}
	return out, firstErr
}
