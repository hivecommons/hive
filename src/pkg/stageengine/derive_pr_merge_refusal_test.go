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
		// The merge queue rejected the head once: the hive retries it, so
		// the stage stays 100. Twice on the same base tip: a person's call.
		{name: "native queue rejected", reason: "native-merge-queue-rejected", want: stagelabels.MergeQueue},
		{name: "native queue rejected repeatedly", reason: "native-merge-queue-rejected-repeatedly", want: stagelabels.NeedsDecision, wantHuman: true},
		{name: "native queue rejected repeatedly but needs-rebase", reason: "native-merge-queue-rejected-repeatedly", labels: []string{"needs-rebase"}, want: stagelabels.Fixing},
		{name: "unclassified merge failure", reason: "merge-failed", want: stagelabels.MergeQueue},
		// GitHub requires conversation resolution and a thread the hive may
		// not resolve is open: a person's call, not merge-imminent.
		{name: "unresolved review threads", reason: "unresolved-review-threads", want: stagelabels.NeedsDecision, wantHuman: true},
		{name: "unresolved review threads but needs-rebase", reason: "unresolved-review-threads", labels: []string{"needs-rebase"}, want: stagelabels.Fixing},
		// The hive resolved its own threads; the next pass merges.
		{name: "own review threads resolved", reason: "review-threads-resolved", want: stagelabels.MergeQueue},
		// A refusal nothing explained yet: retried by the hive, not a person's.
		{name: "review threads pending", reason: "review-threads-pending", want: stagelabels.MergeQueue},
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
