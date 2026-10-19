package stageengine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Transition is what Reconcile reports to its caller and the audit log.
type Transition struct {
	Ref      Ref
	From, To stagelabels.Stage
	Reason   string
	Anomaly  string // "" when expected; otherwise why the from→to pair is unexpected
	Outcome  stagelabels.Outcome
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
	Mode    Mode
	Store   *stagelabels.Store // stagelabels.Shared() by default; API unchanged
	Writer  StageWriter
	Issue   Deriver                                   // defaults to DeriveIssue
	PR      Deriver                                   // defaults to DerivePR
	Legacy  func(f Facts) (stagelabels.Stage, string) // shadow-mode comparator; nil in enforce
	Now     func() time.Time
	Logger  *slog.Logger
	Metrics Metrics
}

// ErrNoWriter is returned by an enforce-mode Reconcile with no Writer.
var ErrNoWriter = errors.New("stage engine: enforce mode needs a StageWriter")

type reconciler struct {
	opts Options

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
	return &reconciler{opts: o, anomalies: map[string]bool{}, mismatch: map[string]bool{}}
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
	if r.opts.Mode == ModeOff {
		return tr, nil
	}
	if f.ObservedAt.IsZero() {
		f.ObservedAt = r.opts.Now()
	}
	unlock := r.lock(f.Ref.Key())
	defer unlock()

	d := r.deriver(f.Ref.Kind).Derive(f)
	if d.Abstain {
		tr.Reason = d.Reason
		return tr, nil
	}
	r.opts.Metrics.Derivation(f.Ref.Kind, d.Stage)

	// What gets written: the engine's Decision in enforce mode; in shadow
	// mode the legacy result (when a comparator is configured), never the
	// engine's.
	want := d
	switch r.opts.Mode {
	case ModeShadow:
		if r.opts.Legacy == nil {
			return r.shadowOnly(tr, d), nil
		}
		legacyStage, legacyReason := r.opts.Legacy(f)
		if legacyStage != d.Stage {
			r.recordMismatch(f, legacyStage, d)
		}
		want.Stage, want.Reason = legacyStage, legacyReason
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

func (r *reconciler) recordMismatch(f Facts, legacy stagelabels.Stage, d Decision) {
	r.opts.Metrics.ShadowMismatch(f.Ref.Kind, legacy, d.Stage)
	k := f.Ref.Key() + "|" + legacy.String() + "→" + d.Stage.String()
	r.mu.Lock()
	seen := r.mismatch[k]
	r.mismatch[k] = true
	r.mu.Unlock()
	if seen {
		return
	}
	r.opts.Logger.Warn("stage engine: shadow mismatch",
		slog.String("kind", f.Ref.Kind.String()), slog.String("ref", f.Ref.String()),
		slog.String("legacy", legacy.String()), slog.String("engine", d.Stage.String()), slog.String("engine_reason", d.Reason))
}

// ReconcileBatch implements Reconciler. The last Facts for a Ref wins; the
// first occurrence fixes its position. Every item is attempted; the first
// error is returned with the transitions of the items that succeeded.
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
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		out = append(out, tr)
	}
	return out, firstErr
}
