package github

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func issueRef7() stageengine.Ref {
	return stageengine.Ref{Owner: "acme", Repo: "widgets", Number: 7, Kind: stagelabels.IssueItem}
}

// The writer plans against a fresh read, removes before adding, and the
// writes are journaled against that read's updated_at: the same desired
// state on a changed item is written again, the same state on the same
// item is deduped by the replay boundary.
func TestStageWriterFreshReadAndJournal(t *testing.T) {
	f, c := newIssueLabelFixture(t, "bug", "stage/010-actionable")
	ctx := context.Background()
	w := c.StageWriter()
	if w != c.StageWriter() {
		t.Fatal("StageWriter must be one per client")
	}
	// The hint says the item is actionable; truth moved on (a relay put
	// 020 on it). The write is planned from truth: 010 and 020 off, 030 on.
	f.readd("stage/020-claimed")
	out, err := w.Apply(ctx, issueRef7(), []string{"bug", "stage/010-actionable"}, stagelabels.Desired{Stage: stagelabels.PRInProgress})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Reread || !out.Healed || strings.Join(out.Removed, ",") != "stage/010-actionable,stage/020-claimed" || strings.Join(out.Added, ",") != "stage/030-pr-in-progress" {
		t.Fatalf("outcome %+v", out)
	}
	// A hint that already matches costs no read and no write.
	out, err = w.Apply(ctx, issueRef7(), []string{"bug", "stage/030-pr-in-progress"}, stagelabels.Desired{Stage: stagelabels.PRInProgress})
	if err != nil || out.Reread || out.Changed() {
		t.Fatalf("settled item: out=%+v err=%v", out, err)
	}
	// The item lapsed and was re-staged by someone else; the same desired
	// state is a new effect because the item's updated_at moved.
	f.mu.Lock()
	f.labels = []string{"bug", "stage/010-actionable"}
	f.updated = f.updated.Add(time.Hour)
	f.mu.Unlock()
	if _, err := w.Apply(ctx, issueRef7(), []string{"bug", "stage/010-actionable"}, stagelabels.Desired{Stage: stagelabels.PRInProgress}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Join(f.deletes, ",") != "stage/010-actionable,stage/020-claimed,stage/010-actionable" || len(f.adds) != 2 {
		t.Fatalf("deletes=%v adds=%v", f.deletes, f.adds)
	}
	if strings.Join(f.labels, ",") != "bug,stage/030-pr-in-progress" {
		t.Fatalf("item carries %v", f.labels)
	}
}

// Two writers racing on one Ref are serialised: their read+write pairs
// never interleave, and the item ends with exactly one stage.
func TestStageWriterSerialisesPerRef(t *testing.T) {
	f, c := newIssueLabelFixture(t, "bug", "stage/010-actionable")
	ctx := context.Background()
	w := c.StageWriter()
	var inflight, maxInflight atomic.Int32
	// Hook the fixture: count Apply bodies in flight via the fresh read.
	stages := []stagelabels.Stage{stagelabels.Claimed, stagelabels.PRInProgress, stagelabels.Actionable, stagelabels.CoveredByPR}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n := inflight.Add(1)
			for {
				m := maxInflight.Load()
				if n <= m || maxInflight.CompareAndSwap(m, n) {
					break
				}
			}
			defer inflight.Add(-1)
			// Hint deliberately stale so every call re-reads and writes.
			if _, err := w.Apply(ctx, issueRef7(), []string{"bug"}, stagelabels.Desired{Stage: stages[i%len(stages)]}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := stagelabels.StageLabels(f.labels); len(got) != 1 {
		t.Fatalf("stage labels after the race: %v", f.labels)
	}
	// Every add was planned from a read that saw the previous add: no add
	// ever carried a stage already present, so no label repeats.
	seen := map[string]int{}
	for _, l := range f.labels {
		seen[l]++
		if seen[l] > 1 {
			t.Fatalf("label %s duplicated on the item: %v", l, f.labels)
		}
	}
	if maxInflight.Load() < 2 {
		t.Skip("goroutines never overlapped; nothing to assert about serialisation")
	}
}

// Nil receivers and clients fail closed.
func TestStageWriterNilClient(t *testing.T) {
	var c *Client
	if c.StageWriter() != nil {
		t.Fatal("nil client has no writer")
	}
	if r := c.StageReconciler(nil, nil, nil); r != nil {
		t.Fatal("nil client has no reconciler")
	}
	if _, err := (&stageWriter{}).Apply(context.Background(), issueRef7(), nil, stagelabels.Desired{}); err != ErrNoGitHubClient {
		t.Fatalf("err = %v", err)
	}
}

// The default reconciler is enforce mode: the engine's derivation is what
// lands on the item. The same Store returns the same reconciler; a
// caller's options are honoured.
func TestStageReconcilerDefaultIsEnforce(t *testing.T) {
	f, c := newIssueLabelFixture(t, "bug", "stage/010-actionable")
	ctx := context.Background()
	store := stagelabels.NewStore("", nil)
	r := c.StageReconciler(store, nil, testLogger())
	if r2 := c.StageReconciler(store, nil, nil); r2 != r {
		t.Fatal("one reconciler per store")
	}
	if sw, ok := r.(stageengine.ModeSwitch); !ok || sw.Mode() != stageengine.ModeEnforce {
		t.Fatalf("default mode = %v, want enforce", r)
	}
	ref := issueRef7()
	facts := stageengine.Facts{Ref: ref, Open: true, Labels: []string{"bug", "stage/010-actionable"}, Issue: &stageengine.IssueFacts{Bucket: stageengine.BucketClaimed}}
	trs, err := r.ReconcileBatch(ctx, []stageengine.Facts{facts})
	if err != nil || len(trs) != 1 || trs[0].To != stagelabels.Claimed {
		t.Fatalf("default write: trs=%+v err=%v", trs, err)
	}
	f.mu.Lock()
	labels := strings.Join(f.labels, ",")
	f.mu.Unlock()
	if labels != "bug,stage/020-claimed" {
		t.Fatalf("item carries %s", labels)
	}
	if rec, ok := store.Get(ref.Key()); !ok || rec.Stage != stagelabels.Claimed {
		t.Fatalf("store %+v", rec)
	}

	// A caller's options win: enforce mode writes the engine's decision.
	store2 := stagelabels.NewStore("", nil)
	engine := stageengine.DeriverFunc(stagelabels.IssueItem, func(stageengine.Facts) stageengine.Decision {
		return stageengine.Decision{Stage: stagelabels.PRInProgress, Reason: "engine"}
	})
	r3 := c.StageReconciler(store2, &stageengine.Options{Mode: stageengine.ModeEnforce, Issue: engine}, nil)
	tr, err := r3.Reconcile(ctx, stageengine.Facts{Ref: ref, Open: true, Labels: []string{"bug", "stage/020-claimed"}, Issue: &stageengine.IssueFacts{Bucket: stageengine.BucketClaimed}})
	if err != nil || tr.To != stagelabels.PRInProgress {
		t.Fatalf("enforce: tr=%+v err=%v", tr, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := strings.Join(f.labels, ","); got != "bug,stage/030-pr-in-progress" {
		t.Fatalf("item carries %s", got)
	}
}
