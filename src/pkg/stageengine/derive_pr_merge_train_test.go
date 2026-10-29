package stageengine

import (
	"testing"

	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#15117: a PR the merge train parked behind an overlapping
// front stays at the merge gate (080) in the hive's hands — even while it
// wears needs-rebase, which the train repairs itself — and only the loop
// guard giving up makes it a person's row (085).
func TestClassifyStageMergeTrainReasons(t *testing.T) {
	files := []string{"stage_tracker.go", "index.html"}
	for _, lane := range []string{stageLaneSelf, stageLaneTrusted, stageLaneContributor} {
		for _, tc := range []struct {
			name   string
			reason string
			want   stagelabels.Stage
			human  bool
		}{
			{"queued-behind", prstage.MergeTrain{State: prstage.MergeTrainStateQueuedBehind, Front: 15074, Position: 3, Size: 4, OverlapFiles: files}.Reason(), stagelabels.AwaitingMergeAdmission, false},
			{"rebasing", prstage.MergeTrain{State: prstage.MergeTrainStateRebasing, Front: 15074, Position: 2, Size: 4, Attempt: 1, MaxAttempts: 2, OverlapFiles: files}.Reason(), stagelabels.AwaitingMergeAdmission, false},
			{"exhausted", prstage.MergeTrain{State: prstage.MergeTrainStateRebaseExhausted, Front: 15074, Position: 2, Size: 4, Attempt: 2, MaxAttempts: 2, OverlapFiles: files}.Reason(), stagelabels.NeedsDecision, true},
		} {
			v := ClassifyStage(stageObservation{Lane: lane, Reason: tc.reason, HeadSHA: goldenHead, Labels: []string{"needs-rebase"}})
			if v.Stage != tc.want || v.Rank != stageRankOwned || v.Human != tc.human || v.Terminal {
				t.Fatalf("%s/%s classified as %+v, want stage %v human %v", lane, tc.name, v, tc.want, tc.human)
			}
		}
	}
}
