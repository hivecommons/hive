package stageengine

import (
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// A merge GitHub refuses with a 405 no retry cures is not "merge imminent":
// a missing approving review or a disabled merge method parks at 085 for a
// person, while a merge-queue-only base and an unclassified merge failure
// stay in the merge queue.
func TestClassifyStagePermanentMergeRefusals(t *testing.T) {
	approve := stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}}
	for _, tc := range []struct {
		name      string
		reason    string
		labels    []string
		want      stagelabels.Stage
		wantHuman bool
	}{
		{name: "review required", reason: autoMergeReasonMergeRefusedReview, want: stagelabels.NeedsDecision, wantHuman: true},
		{name: "method not allowed", reason: autoMergeReasonMergeRefusedMethod, want: stagelabels.NeedsDecision, wantHuman: true},
		{name: "review required but needs-rebase", reason: autoMergeReasonMergeRefusedReview, labels: []string{"needs-rebase"}, want: stagelabels.Fixing},
		{name: "merge queue required", reason: autoMergeReasonMergeQueueRequired, want: stagelabels.MergeQueue},
		{name: "native queued position", reason: "native-merge-queued: queued (position 2, AWAITING_CHECKS)", want: stagelabels.MergeQueue},
		{name: "native enqueue failure", reason: "native-merge-queue-enqueue", want: stagelabels.MergeQueue},
		{name: "native read failure", reason: "native-merge-queue-read", want: stagelabels.MergeQueue},
		{name: "native queue method refused", reason: "native-merge-queue-method", want: stagelabels.NeedsDecision, wantHuman: true},
		{name: "unclassified merge failure", reason: "merge-failed", want: stagelabels.MergeQueue},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, lane := range []string{stageLaneSelf, stageLaneTrusted, stageLaneContributor} {
				got := ClassifyStage(stageObservation{Lane: lane, HeadSHA: "sha", Reason: tc.reason, Labels: tc.labels, Detail: approve})
				want := tc.want
				if want == stagelabels.Fixing {
					want = FixingStage(stageObservation{Lane: lane})
				}
				if got.Stage != want || got.Rank != stageRankOwned || got.Human != tc.wantHuman {
					t.Fatalf("%s lane: got %+v, want stage %v rank owned human=%v", lane, got, want, tc.wantHuman)
				}
			}
		})
	}
}
