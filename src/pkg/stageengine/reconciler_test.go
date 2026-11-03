package stageengine

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// fakeWriter is an in-memory StageWriter: it keeps each item's labels and
// applies stagelabels.Enforce against them, the same plan the real writer
// will run. Writes counts every Apply that changed a label.
type fakeWriter struct {
	mu     sync.Mutex
	labels map[string][]string
	Writes int
	Err    error
}

func newFakeWriter() *fakeWriter { return &fakeWriter{labels: map[string][]string{}} }

func (w *fakeWriter) Labels(_ context.Context, owner, repo string, number int) ([]string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.labels[stagelabels.Key(owner+"/"+repo, number)]...), nil
}

func (w *fakeWriter) AddLabels(_ context.Context, owner, repo string, number int, labels []string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	k := stagelabels.Key(owner+"/"+repo, number)
	// A forge's label set has no duplicates: adding a present label is a no-op.
	for _, l := range labels {
		if !slices.Contains(w.labels[k], l) {
			w.labels[k] = append(w.labels[k], l)
		}
	}
	return nil
}

func (w *fakeWriter) RemoveLabel(_ context.Context, owner, repo string, number int, label string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	k := stagelabels.Key(owner+"/"+repo, number)
	var kept []string
	for _, l := range w.labels[k] {
		if l != label {
			kept = append(kept, l)
		}
	}
	w.labels[k] = kept
	return nil
}

func (w *fakeWriter) Apply(ctx context.Context, ref Ref, hint []string, want stagelabels.Desired) (stagelabels.Outcome, error) {
	if w.Err != nil {
		return stagelabels.Outcome{}, w.Err
	}
	out, err := stagelabels.Enforce(ctx, w, ref.Kind, ref.Owner, ref.Repo, ref.Number, hint, want)
	if out.Changed() {
		w.mu.Lock()
		w.Writes++
		w.mu.Unlock()
	}
	return out, err
}

func (w *fakeWriter) stageLabels(ref Ref) []string {
	labels, _ := w.Labels(context.Background(), ref.Owner, ref.Repo, ref.Number)
	return stagelabels.StageLabels(labels)
}

// tableDeriver derives from Facts.Issue.Bucket / Facts.PR.Lanes[0].Reason
// through a fixed table: enough variety for the property test without
// depending on the adapters in pkg/github.
func tableDeriver(k stagelabels.Item) Deriver {
	return DeriverFunc(k, func(f Facts) Decision {
		if !f.Open {
			return Decision{Terminal: true, Reason: "closed"}
		}
		if k == stagelabels.IssueItem {
			if f.Issue == nil {
				return abstain("no issue facts")
			}
			switch f.Issue.Bucket {
			case BucketActionable:
				return Decision{Stage: stagelabels.Actionable, Reason: "actionable"}
			case BucketClaimed:
				return Decision{Stage: stagelabels.Claimed, Reason: "claimed"}
			case BucketInProgress:
				return Decision{Stage: stagelabels.PRInProgress, Reason: "kick"}
			case BucketCoveredByPR:
				return Decision{Stage: stagelabels.CoveredByPR, Reason: "covered"}
			}
			return Decision{Reason: "filtered"}
		}
		if f.PR == nil || len(f.PR.Lanes) == 0 {
			return abstain("no lane")
		}
		switch f.PR.Lanes[0].Reason {
		case "pending":
			return Decision{Stage: stagelabels.ReviewPending, Role: stagelabels.RoleTrusted, Kind: f.Author.Kind}
		case "red":
			return Decision{Stage: stagelabels.FixingStageFor(f.Author.SelfAuthored, f.Author.Kind), Kind: f.Author.Kind}
		case "soaking":
			return Decision{Stage: stagelabels.MergeQueue, Kind: f.Author.Kind, DCO: f.PR.DCO}
		case "untrusted":
			return Decision{Stage: stagelabels.ReadyForHumanMerge, Role: stagelabels.RoleUntrusted, Human: &stagelabels.Route{Stage: stagelabels.ReadyForHumanMerge}}
		}
		return Decision{Stage: stagelabels.NeedsDecision, Human: &stagelabels.Route{Stage: stagelabels.NeedsDecision}}
	})
}

func randomFacts(rng *rand.Rand, n int) Facts {
	kinds := []authorkind.Kind{authorkind.Unknown, authorkind.Human, authorkind.Clanker, authorkind.Hive}
	f := Facts{
		Ref:    Ref{Owner: "acme", Repo: "widgets", Number: n},
		Open:   rng.Intn(8) != 0,
		Author: AuthorFacts{Login: "someone", Kind: kinds[rng.Intn(len(kinds))], SelfAuthored: rng.Intn(3) == 0},
	}
	// Pre-existing labels another writer left behind, including duplicates.
	stray := []string{stagelabels.Actionable.Label(), stagelabels.ReviewPending.Label(), stagelabels.MergeQueue.Label(), "bug", stagelabels.StalledLabel, "role/trusted"}
	for i := 0; i < rng.Intn(4); i++ {
		f.Labels = append(f.Labels, stray[rng.Intn(len(stray))])
	}
	if rng.Intn(2) == 0 {
		f.Ref.Kind = stagelabels.IssueItem
		buckets := []IssueBucket{BucketActionable, BucketClaimed, BucketInProgress, BucketCoveredByPR, BucketFiltered, "reporter_triage"}
		f.Issue = &IssueFacts{Bucket: buckets[rng.Intn(len(buckets))]}
	} else {
		f.Ref.Kind = stagelabels.PRItem
		reasons := []string{"pending", "red", "soaking", "untrusted", "hold"}
		f.PR = &PRFacts{HeadSHA: "abc", DCO: stagelabels.DCOState(rng.Intn(3)), Lanes: []LaneFact{{Lane: "self-authored", Reason: reasons[rng.Intn(len(reasons))]}}}
	}
	return f
}

func newTestReconciler(t *testing.T, mode Mode, w StageWriter) (Reconciler, *Counters, *stagelabels.Store) {
	t.Helper()
	store := stagelabels.NewStore("", nil)
	m := NewCounters()
	r := New(Options{
		Mode:    mode,
		Store:   store,
		Writer:  w,
		Issue:   tableDeriver(stagelabels.IssueItem),
		PR:      tableDeriver(stagelabels.PRItem),
		Now:     func() time.Time { return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC) },
		Logger:  slog.New(slog.NewTextHandler(&strings.Builder{}, nil)),
		Metrics: m,
	})
	return r, m, store
}

// TestReconcileIdempotent: for random Facts, Reconcile twice in enforce
// mode against a fake writer → the second call writes nothing and the item
// carries exactly one stage label (or none, when the Decision is None).
func TestReconcileIdempotent(t *testing.T) {
	rng := rand.New(rand.NewSource(12999))
	ctx := context.Background()
	for i := 1; i <= 300; i++ {
		w := newFakeWriter()
		r, _, _ := newTestReconciler(t, ModeEnforce, w)
		f := randomFacts(rng, i)
		w.labels[f.Ref.Key()] = append([]string(nil), f.Labels...)

		first, err := r.Reconcile(ctx, f)
		if err != nil {
			t.Fatalf("#%d first reconcile: %v", i, err)
		}
		writes := w.Writes
		// The second pass sees what the first wrote, as a sweep would.
		f.Labels, _ = w.Labels(ctx, f.Ref.Owner, f.Ref.Repo, f.Ref.Number)
		second, err := r.Reconcile(ctx, f)
		if err != nil {
			t.Fatalf("#%d second reconcile: %v", i, err)
		}
		if w.Writes != writes || second.Outcome.Changed() {
			t.Fatalf("#%d second reconcile wrote %+v (facts %+v)", i, second.Outcome, f)
		}
		stages := w.stageLabels(f.Ref)
		d := tableDeriver(f.Ref.Kind).Derive(f)
		switch {
		case d.Abstain:
			// Nothing touched: whatever was there stays.
		case d.Stage == stagelabels.None:
			if len(stages) != 0 {
				t.Fatalf("#%d None decision left stage labels %v", i, stages)
			}
		default:
			if len(stages) != 1 || stages[0] != d.Stage.For(f.Ref.Kind).Label() {
				t.Fatalf("#%d stage labels %v, want exactly %s", i, stages, d.Stage.Label())
			}
			if first.To != d.Stage || second.From != d.Stage || second.To != d.Stage {
				t.Fatalf("#%d transitions first=%+v second=%+v", i, first, second)
			}
		}
	}
}

func prFacts(n int, reason string) Facts {
	return Facts{
		Ref:    Ref{Owner: "acme", Repo: "widgets", Number: n, Kind: stagelabels.PRItem},
		Open:   true,
		Labels: []string{"bug"},
		Author: AuthorFacts{Kind: authorkind.Human},
		PR:     &PRFacts{HeadSHA: "abc", Lanes: []LaneFact{{Lane: "trusted-author", Reason: reason}}},
	}
}

// Shadow: pure compute, nothing observed or written.
func TestShadowWritesNothing(t *testing.T) {
	ctx := context.Background()
	w := newFakeWriter()
	r, m, store := newTestReconciler(t, ModeShadow, w)
	f := prFacts(8, "soaking")
	tr, err := r.Reconcile(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if w.Writes != 0 || len(w.stageLabels(f.Ref)) != 0 {
		t.Fatalf("shadow wrote %v", w.labels)
	}
	if _, ok := store.Get(f.Ref.Key()); ok {
		t.Fatal("shadow must not observe the store")
	}
	if tr.To != stagelabels.MergeQueue || m.Total(MetricDerivations) != 1 {
		t.Fatalf("transition %+v metrics %+v", tr, m.Snapshot())
	}
}

func TestEnforceNeedsWriter(t *testing.T) {
	r, _, _ := newTestReconciler(t, ModeEnforce, nil)
	if _, err := r.Reconcile(context.Background(), prFacts(1, "soaking")); !errors.Is(err, ErrNoWriter) {
		t.Fatalf("err = %v, want ErrNoWriter", err)
	}
}

func TestEnforceAnomalyIsCountedNotRejected(t *testing.T) {
	ctx := context.Background()
	w := newFakeWriter()
	r, m, store := newTestReconciler(t, ModeEnforce, w)
	f := prFacts(3, "soaking") // 100
	if _, err := r.Reconcile(ctx, f); err != nil {
		t.Fatal(err)
	}
	f.PR.Lanes[0].Reason = "pending" // 100 → 050: a regression the diagram does not draw
	f.Labels = []string{"bug", stagelabels.MergeQueue.Label()}
	tr, err := r.Reconcile(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Anomaly == "" || tr.From != stagelabels.MergeQueue || tr.To != stagelabels.ReviewPending {
		t.Fatalf("transition %+v should be an anomaly", tr)
	}
	if got := w.stageLabels(f.Ref); len(got) != 1 || got[0] != stagelabels.ReviewPending.Label() {
		t.Fatalf("anomaly must still be applied; labels %v", got)
	}
	if rec, _ := store.Get(f.Ref.Key()); rec.Stage != stagelabels.ReviewPending {
		t.Fatalf("store %+v", rec)
	}
	if m.Total(MetricAnomaly) != 1 {
		t.Fatalf("anomaly counter %d", m.Total(MetricAnomaly))
	}
	// The same stage again is a no-op (050 → 050), never a second anomaly.
	f.Labels = []string{"bug", stagelabels.ReviewPending.Label()}
	if _, err := r.Reconcile(ctx, f); err != nil {
		t.Fatal(err)
	}
	if m.Total(MetricAnomaly) != 1 {
		t.Fatalf("anomaly counter %d after no-op", m.Total(MetricAnomaly))
	}
	// The same regression on another item counts again and logs again;
	// on the same item it counts but is logged once.
	g := prFacts(30, "soaking")
	if _, err := r.Reconcile(ctx, g); err != nil {
		t.Fatal(err)
	}
	g.PR.Lanes[0].Reason = "pending"
	for i := 0; i < 2; i++ {
		g.Labels = w.stageLabels(g.Ref)
		if _, err := r.Reconcile(ctx, g); err != nil {
			t.Fatal(err)
		}
		g.PR.Lanes[0].Reason = "soaking"
	}
	if m.Total(MetricAnomaly) != 2 {
		t.Fatalf("anomaly counter %d, want 2", m.Total(MetricAnomaly))
	}
}

func TestEnforceWritesMetricsAndHealsDuplicates(t *testing.T) {
	ctx := context.Background()
	w := newFakeWriter()
	r, m, _ := newTestReconciler(t, ModeEnforce, w)
	f := prFacts(4, "pending")
	f.Labels = []string{stagelabels.MergeQueue.Label(), stagelabels.Reviewed.Label()}
	w.labels[f.Ref.Key()] = append([]string(nil), f.Labels...)
	tr, err := r.Reconcile(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if !tr.Outcome.Healed || len(tr.Outcome.Removed) != 2 || len(tr.Outcome.Added) < 1 {
		t.Fatalf("outcome %+v", tr.Outcome)
	}
	if m.Total(MetricDuplicateHealed) != 1 || m.Total(MetricWriterWrites) != len(tr.Outcome.Added)+len(tr.Outcome.Removed) {
		t.Fatalf("metrics %+v", m.Snapshot())
	}
	// Terminal: every hive-owned label comes off and the record is dropped.
	f.Open = false
	f.Labels, _ = w.Labels(ctx, "acme", "widgets", 4)
	tr, err = r.Reconcile(ctx, f)
	if err != nil || tr.To != stagelabels.None || len(w.stageLabels(f.Ref)) != 0 {
		t.Fatalf("terminal: tr=%+v err=%v labels=%v", tr, err, w.labels)
	}
}

func TestWriterErrorIsReported(t *testing.T) {
	w := newFakeWriter()
	w.Err = errors.New("boom")
	r, _, _ := newTestReconciler(t, ModeEnforce, w)
	_, err := r.Reconcile(context.Background(), prFacts(5, "pending"))
	if err == nil || !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "acme/widgets#5") {
		t.Fatalf("err = %v", err)
	}
}

func TestAbstainTouchesNothing(t *testing.T) {
	w := newFakeWriter()
	r, m, store := newTestReconciler(t, ModeEnforce, w)
	f := prFacts(6, "pending")
	f.PR.Lanes = nil
	tr, err := r.Reconcile(context.Background(), f)
	if err != nil || w.Writes != 0 || m.Total(MetricDerivations) != 0 || len(store.Keys()) != 0 {
		t.Fatalf("abstain: tr=%+v err=%v writes=%d", tr, err, w.Writes)
	}
	if tr.Reason != "no lane" {
		t.Fatalf("reason %q", tr.Reason)
	}
}

func TestReconcileBatchDedupesAndKeepsOrder(t *testing.T) {
	ctx := context.Background()
	w := newFakeWriter()
	r, _, _ := newTestReconciler(t, ModeEnforce, w)
	a := prFacts(1, "pending")
	b := prFacts(2, "soaking")
	a2 := prFacts(1, "soaking") // later facts for #1 win, position stays first
	trs, err := r.ReconcileBatch(ctx, []Facts{a, b, a2})
	if err != nil {
		t.Fatal(err)
	}
	if len(trs) != 2 || trs[0].Ref.Number != 1 || trs[1].Ref.Number != 2 {
		t.Fatalf("transitions %+v", trs)
	}
	if trs[0].To != stagelabels.MergeQueue {
		t.Fatalf("#1 got %s, want the later facts' 100", trs[0].To)
	}
	// A failing item does not stop the others; its error is returned.
	w.Err = errors.New("boom")
	trs, err = r.ReconcileBatch(ctx, []Facts{a})
	if err == nil || len(trs) != 1 || trs[0].Err == nil {
		t.Fatalf("trs=%v err=%v", trs, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := r.ReconcileBatch(cancelled, []Facts{a}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled batch err = %v", err)
	}
}

// TestConcurrentReconcileOneStage: interleaved reconciles of one item (a
// FactSink submission racing a sweep) end with exactly one stage label.
func TestConcurrentReconcileOneStage(t *testing.T) {
	ctx := context.Background()
	w := newFakeWriter()
	r, _, _ := newTestReconciler(t, ModeEnforce, w)
	sink := NewFactSink(r, FactSinkOptions{})
	ref := Ref{Owner: "acme", Repo: "widgets", Number: 42, Kind: stagelabels.IssueItem}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = sink.Claimed(ctx, ref, ClaimFact{Holder: "bob"})
		}()
		go func() {
			defer wg.Done()
			_, _ = r.ReconcileBatch(ctx, []Facts{{Ref: ref, Open: true, Issue: &IssueFacts{Bucket: BucketActionable}}})
		}()
	}
	wg.Wait()
	if got := w.stageLabels(ref); len(got) != 1 {
		t.Fatalf("stage labels after interleaving: %v", got)
	}
}

func TestNewDefaults(t *testing.T) {
	r := New(Options{}).(*reconciler)
	if r.opts.Mode != DefaultMode || r.opts.Store == nil || r.opts.Issue == nil || r.opts.PR == nil || r.opts.Now == nil || r.opts.Logger == nil || r.opts.Metrics != DefaultCounters() {
		t.Fatalf("defaults not applied: %+v", r.opts)
	}
}

// suppressingWriter is a StageWriter standing in for the write-back gate
// refusing a contributor PR's labels: Apply reports Suppressed and lands
// nothing, as github.stageWriter does when the gate says no.
type suppressingWriter struct {
	suppress bool
	err      error
}

func (w *suppressingWriter) Apply(context.Context, Ref, []string, stagelabels.Desired) (stagelabels.Outcome, error) {
	return stagelabels.Outcome{Suppressed: w.suppress}, w.err
}

// TestReconcileRecordsSuppressedLabels: a write the gate kept off GitHub
// marks the record so readers know its labels never carried the stage;
// the mark clears once a write lands, and a failed write leaves it as is.
func TestReconcileRecordsSuppressedLabels(t *testing.T) {
	ctx := context.Background()
	w := &suppressingWriter{suppress: true}
	r, _, store := newTestReconciler(t, ModeEnforce, w)
	f := prFacts(15312, "pending")
	if _, err := r.Reconcile(ctx, f); err != nil {
		t.Fatal(err)
	}
	rec, ok := store.Get(f.Ref.Key())
	if !ok || rec.Stage == stagelabels.None || !rec.LabelsSuppressed {
		t.Fatalf("suppressed write: record = %+v ok=%t, want stage kept with labels_suppressed", rec, ok)
	}
	w.err = errors.New("boom")
	w.suppress = false
	if _, err := r.Reconcile(ctx, f); err == nil {
		t.Fatal("expected the writer's error")
	}
	if rec, _ := store.Get(f.Ref.Key()); !rec.LabelsSuppressed {
		t.Fatalf("failed write cleared labels_suppressed: %+v", rec)
	}
	w.err = nil
	if _, err := r.Reconcile(ctx, f); err != nil {
		t.Fatal(err)
	}
	if rec, _ := store.Get(f.Ref.Key()); rec.LabelsSuppressed {
		t.Fatalf("landed write left labels_suppressed set: %+v", rec)
	}
}
