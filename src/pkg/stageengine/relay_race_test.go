package stageengine

import (
	"context"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// TestRelayVsSweepOneStage is the PR-C race (run under -race): a relay
// claim submitted through the FactSink, a legacy-style relay that still
// drops stage/020 straight onto the item, and the sweep's ReconcileBatch
// all hit one issue at once, N times. The item ends with exactly one stage
// label, every transition is one the pipeline draws (010→020 or a
// no-op), and a further reconcile writes nothing.
func TestRelayVsSweepOneStage(t *testing.T) {
	ctx := context.Background()
	w := newFakeWriter()
	r, m, _ := newTestReconciler(t, ModeEnforce, w)
	sink := NewFactSink(r, FactSinkOptions{})
	ref := Ref{Owner: "acme", Repo: "widgets", Number: 77, Kind: stagelabels.IssueItem}
	w.labels[ref.Key()] = []string{"bug", stagelabels.Actionable.Label()}

	var (
		mu          sync.Mutex
		transitions []Transition
	)
	record := func(trs ...Transition) {
		mu.Lock()
		transitions = append(transitions, trs...)
		mu.Unlock()
	}
	claim := ClaimFact{Holder: "clanker-7", Kind: claims.KindAgent}
	sweep := Facts{Ref: ref, Open: true, Labels: []string{"bug", stagelabels.Actionable.Label()}, Issue: &IssueFacts{Bucket: BucketClaimed, Claim: &claim}}

	const rounds = 64
	var wg sync.WaitGroup
	for i := 0; i < rounds; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			if err := sink.Claimed(ctx, ref, claim); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			// Today's relay: a bare label add behind the engine's back.
			_ = w.AddLabels(ctx, ref.Owner, ref.Repo, ref.Number, []string{stagelabels.Claimed.Label()})
		}()
		go func() {
			defer wg.Done()
			trs, err := r.ReconcileBatch(ctx, []Facts{sweep})
			if err != nil {
				t.Error(err)
			}
			record(trs...)
		}()
	}
	wg.Wait()

	// The relay's bare adds may have landed after the last engine write;
	// the next sweep heals them. From here on nothing else touches the item.
	tr, err := r.Reconcile(ctx, sweep)
	if err != nil {
		t.Fatal(err)
	}
	record(tr)
	if got := w.stageLabels(ref); len(got) != 1 || got[0] != stagelabels.Claimed.Label() {
		t.Fatalf("stage labels after the race: %v, want exactly %s", got, stagelabels.Claimed.Label())
	}
	for _, tr := range transitions {
		if tr.Anomaly != "" {
			t.Fatalf("unexpected transition %s→%s on %s: %s", tr.From, tr.To, tr.Ref, tr.Anomaly)
		}
		if tr.To != stagelabels.Claimed {
			t.Fatalf("transition to %s, want every write to land on 020", tr.To)
		}
	}
	if m.Total(MetricAnomaly) != 0 {
		t.Fatalf("anomalies counted: %+v", m.Snapshot())
	}

	// Property: a settled item costs nothing. Repeated reconciles write nothing.
	writes := w.Writes
	sweep.Labels, _ = w.Labels(ctx, ref.Owner, ref.Repo, ref.Number)
	for i := 0; i < 5; i++ {
		tr, err := r.Reconcile(ctx, sweep)
		if err != nil {
			t.Fatal(err)
		}
		if tr.Outcome.Changed() {
			t.Fatalf("repeated reconcile #%d wrote %+v", i, tr.Outcome)
		}
		if err := sink.Claimed(ctx, ref, claim); err != nil {
			t.Fatal(err)
		}
	}
	if w.Writes != writes {
		t.Fatalf("repeated reconcile wrote: %d → %d", writes, w.Writes)
	}
}
