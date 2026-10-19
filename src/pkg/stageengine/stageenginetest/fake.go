// Package stageenginetest is the test double for stageengine.FactSink: a
// sink backed by the real Reconciler (enforce mode, a fresh Store) and an
// in-memory StageWriter that plans with stagelabels.Enforce exactly as the
// production writer does, so a test asserts the labels a fact ends up as
// rather than the method a writer called.
package stageenginetest

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Apply is one StageWriter.Apply call the Writer saw.
type Apply struct {
	Ref  stageengine.Ref
	Want stagelabels.Desired
	Out  stagelabels.Outcome
}

// Writer is an in-memory StageWriter: it keeps each item's labels, applies
// a Desired with stagelabels.Enforce against them and records every call.
type Writer struct {
	mu      sync.Mutex
	labels  map[string][]string
	applies []Apply
	// Err, when set, fails every Apply.
	Err error
}

// NewWriter returns an empty Writer.
func NewWriter() *Writer { return &Writer{labels: map[string][]string{}} }

// Seed sets the labels an item carries before any fact arrives.
func (w *Writer) Seed(ref stageengine.Ref, labels ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.labels[key(ref.Owner, ref.Repo, ref.Number)] = append([]string(nil), labels...)
}

// LabelsOf returns the labels the item carries now.
func (w *Writer) LabelsOf(ref stageengine.Ref) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.labels[key(ref.Owner, ref.Repo, ref.Number)]...)
}

// Applies returns every Apply call so far.
func (w *Writer) Applies() []Apply {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Apply(nil), w.applies...)
}

// Apply implements stageengine.StageWriter.
func (w *Writer) Apply(ctx context.Context, ref stageengine.Ref, hint []string, want stagelabels.Desired) (stagelabels.Outcome, error) {
	if w.Err != nil {
		return stagelabels.Outcome{}, w.Err
	}
	// The fake plans against its own truth, not the caller's hint, so a
	// stale hint never hides a write the production writer would make.
	out, err := stagelabels.Enforce(ctx, labeler{w}, ref.Kind, ref.Owner, ref.Repo, ref.Number, w.LabelsOf(ref), want)
	w.mu.Lock()
	w.applies = append(w.applies, Apply{Ref: ref, Want: want, Out: out})
	w.mu.Unlock()
	return out, err
}

type labeler struct{ w *Writer }

func (l labeler) Labels(_ context.Context, owner, repo string, number int) ([]string, error) {
	l.w.mu.Lock()
	defer l.w.mu.Unlock()
	return append([]string(nil), l.w.labels[key(owner, repo, number)]...), nil
}

func (l labeler) AddLabels(_ context.Context, owner, repo string, number int, labels []string) error {
	l.w.mu.Lock()
	defer l.w.mu.Unlock()
	k := key(owner, repo, number)
	for _, label := range labels {
		if !has(l.w.labels[k], label) {
			l.w.labels[k] = append(l.w.labels[k], label)
		}
	}
	return nil
}

func (l labeler) RemoveLabel(_ context.Context, owner, repo string, number int, label string) error {
	l.w.mu.Lock()
	defer l.w.mu.Unlock()
	k := key(owner, repo, number)
	kept := l.w.labels[k][:0]
	for _, have := range l.w.labels[k] {
		if have != label {
			kept = append(kept, have)
		}
	}
	l.w.labels[k] = kept
	return nil
}

func key(owner, repo string, number int) string { return stagelabels.Key(owner+"/"+repo, number) }

func has(labels []string, label string) bool {
	for _, l := range labels {
		if l == label {
			return true
		}
	}
	return false
}

// Options tunes NewSink.
type Options struct {
	// Issue and PR are the derivers; nil uses the engine's registered ones
	// (stageengine.IssueDeriver / PRDeriver), which abstain until a
	// composition root calls stageengine.SetDerivers.
	Issue, PR stageengine.Deriver
	// Enrich is the sink's Enrich hook. The default marks the item open and
	// tops its labels up with what the Writer holds, standing in for the
	// production sink's fresh read.
	Enrich func(ctx context.Context, f stageengine.Facts) (stageengine.Facts, error)
	Now    func() time.Time
}

// NewSink returns a FactSink over the real Reconciler in enforce mode, a
// fresh Store and the returned Writer.
func NewSink(opts Options) (stageengine.FactSink, *Writer) {
	w := NewWriter()
	if opts.Enrich == nil {
		opts.Enrich = func(_ context.Context, f stageengine.Facts) (stageengine.Facts, error) {
			f.Open = true
			for _, l := range w.LabelsOf(f.Ref) {
				if !has(f.Labels, l) {
					f.Labels = append(f.Labels, l)
				}
			}
			if f.Issue != nil && f.Park.ApprovedDirection {
				f.Issue.Bucket = stageengine.BucketActionable
			}
			return f, nil
		}
	}
	r := stageengine.New(stageengine.Options{
		Mode:   stageengine.ModeEnforce,
		Store:  stagelabels.NewStore("", opts.Now),
		Writer: w,
		Issue:  opts.Issue,
		PR:     opts.PR,
		Now:    opts.Now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return stageengine.NewFactSink(r, stageengine.FactSinkOptions{Now: opts.Now, Enrich: opts.Enrich}), w
}

// Recorder is a FactSink that only records the facts it is handed, for
// tests that assert a writer submitted the right fact and nothing else.
type Recorder struct {
	mu    sync.Mutex
	Facts []Recorded
	// Err, when set, is returned by every method.
	Err error
}

// Recorded is one FactSink call.
type Recorded struct {
	Method string // Claimed | ClaimReleased | InProgress | CoveredBy | Uncovered | Parked | Unparked
	Ref    stageengine.Ref
	Claim  *stageengine.ClaimFact
	PR     *stageengine.Ref
	Who    string // holder / agent / by
	Label  string
	Until  time.Time
}

func (r *Recorder) record(rec Recorded) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Facts = append(r.Facts, rec)
	return r.Err
}

// All returns the recorded facts.
func (r *Recorder) All() []Recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Recorded(nil), r.Facts...)
}

func (r *Recorder) Claimed(_ context.Context, ref stageengine.Ref, c stageengine.ClaimFact) error {
	return r.record(Recorded{Method: "Claimed", Ref: ref, Claim: &c, Who: c.Holder})
}
func (r *Recorder) ClaimReleased(_ context.Context, ref stageengine.Ref, holder string) error {
	return r.record(Recorded{Method: "ClaimReleased", Ref: ref, Who: holder})
}
func (r *Recorder) InProgress(_ context.Context, ref stageengine.Ref, agent string, until time.Time) error {
	return r.record(Recorded{Method: "InProgress", Ref: ref, Who: agent, Until: until})
}
func (r *Recorder) CoveredBy(_ context.Context, issue, pr stageengine.Ref) error {
	return r.record(Recorded{Method: "CoveredBy", Ref: issue, PR: &pr})
}
func (r *Recorder) Uncovered(_ context.Context, issue, pr stageengine.Ref) error {
	return r.record(Recorded{Method: "Uncovered", Ref: issue, PR: &pr})
}
func (r *Recorder) Parked(_ context.Context, ref stageengine.Ref, label string) error {
	return r.record(Recorded{Method: "Parked", Ref: ref, Label: label})
}
func (r *Recorder) Unparked(_ context.Context, ref stageengine.Ref, by string) error {
	return r.record(Recorded{Method: "Unparked", Ref: ref, Who: by})
}

var _ stageengine.FactSink = (*Recorder)(nil)
var _ stageengine.StageWriter = (*Writer)(nil)
