package dashboard

import (
	"slices"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// A PR the sweep held out of the merge queue as a duplicate
// (hivecommons/hive#15395) reads "duplicate-of:<winner>" on the gate with
// the winner and issue beside it, rides no train, and wears
// needs-decision so the row offers "Let hive decide".
func TestStageTrackerApplyDuplicateHold(t *testing.T) {
	reason := prstage.MergeTrain{State: prstage.MergeTrainStateDuplicateOf, Front: 15402, Position: 15396}.Reason()
	it := StageTrackerItem{Repo: "hivecommons/hive", Kind: "pr", Number: 15407, Stage: 80, BaseRef: "v5", Reason: reason, Modifiers: []string{modNeedsRebase}}
	stageTrackerApplyMergeTrain(&it)
	if it.MergeTrain != nil {
		t.Fatalf("a held duplicate rides no train: %+v", it.MergeTrain)
	}
	if it.MergeGate == nil || it.MergeGate.State != "duplicate-of:15402" || it.MergeGate.DuplicateOf != 15402 || it.MergeGate.DuplicateIssue != 15396 {
		t.Fatalf("gate = %+v", it.MergeGate)
	}
	if it.MergeGate.Reason != "duplicate of #15402 for issue #15396; superseded by #15402 — the merge queue carries one PR per issue; close it or let the delegate decide" {
		t.Fatalf("reason = %q", it.MergeGate.Reason)
	}
	if !slices.Contains(it.Modifiers, modNeedsDecision) || !slices.Contains(it.Modifiers, modNeedsRebase) {
		t.Fatalf("modifiers = %v", it.Modifiers)
	}
	// Applying twice does not stack the modifier; the post-pass ignores it.
	stageTrackerApplyMergeTrain(&it)
	if n := slices.Index(it.Modifiers, modNeedsDecision); n < 0 || slices.Contains(it.Modifiers[n+1:], modNeedsDecision) {
		t.Fatalf("modifiers = %v", it.Modifiers)
	}
	items := []StageTrackerItem{it}
	stageTrackerMergeTrainPostPass(items, mergeTrainQueueOptions{Now: time.Now()}, &mergeTrainMemory{})
	if items[0].MergeTrain != nil {
		t.Fatalf("post-pass gave the held row a train: %+v", items[0].MergeTrain)
	}
}

func TestDelegatedTrainDuplicateReadsStore(t *testing.T) {
	now := time.Date(2026, 10, 29, 15, 0, 0, 0, time.UTC)
	store := stagelabels.NewStore("", func() time.Time { return now })
	store.Observe(stagelabels.Key("hivecommons/hive", 15407), stagelabels.AwaitingMergeAdmission,
		prstage.MergeTrain{State: prstage.MergeTrainStateDuplicateOf, Front: 15402, Position: 15396}.Reason())
	store.Observe(stagelabels.Key("hivecommons/hive", 15402), stagelabels.AwaitingMergeAdmission,
		prstage.MergeTrain{State: prstage.MergeTrainStateQueuedBehind, Front: 15315, Position: 2, Size: 3}.Reason())
	if w, i := delegatedTrainDuplicate(store, "hivecommons/hive", 15407); w != 15402 || i != 15396 {
		t.Fatalf("held = #%d issue #%d", w, i)
	}
	if w, i := delegatedTrainDuplicate(store, "hivecommons/hive", 15402); w != 0 || i != 0 {
		t.Fatalf("a queued member is not held: #%d #%d", w, i)
	}
	if w, _ := delegatedTrainDuplicate(nil, "hivecommons/hive", 15407); w != 0 {
		t.Fatal("nil store")
	}
}
