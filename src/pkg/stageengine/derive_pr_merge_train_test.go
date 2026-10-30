package stageengine

import (
	"testing"

	"github.com/hivecommons/hive/pkg/authorkind"
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
			// hivecommons/hive#15825: a head the train is rebasing has no
			// CI and no verdicts yet — being fixed, never merge-queued.
			{"rebasing", prstage.MergeTrain{State: prstage.MergeTrainStateRebasing, Front: 15074, Position: 2, Size: 4, Attempt: 1, MaxAttempts: 2, OverlapFiles: files}.Reason(), stagelabels.Fixing, false},
			{"exhausted", prstage.MergeTrain{State: prstage.MergeTrainStateRebaseExhausted, Front: 15074, Position: 2, Size: 4, Attempt: 2, MaxAttempts: 2, OverlapFiles: files}.Reason(), stagelabels.NeedsDecision, true},
			// hivecommons/hive#15810: an undecided same-issue claim across
			// lanes is a person's call, not a turn in the queue.
			{"claim-conflict", prstage.MergeTrain{State: prstage.MergeTrainStateClaimConflict, Front: 15793, Position: 15773}.Reason(), stagelabels.NeedsDecision, true},
		} {
			v := ClassifyStage(stageObservation{Lane: lane, Reason: tc.reason, HeadSHA: goldenHead, Labels: []string{"needs-rebase"}})
			if v.Stage != tc.want || v.Rank != stageRankOwned || v.Human != tc.human || v.Terminal {
				t.Fatalf("%s/%s classified as %+v, want stage %v human %v", lane, tc.name, v, tc.want, tc.human)
			}
		}
	}
}

// hivecommons/hive#15469: a PR the queue holds because another open PR
// fixes the same issue is not "waiting its turn" — it goes back to its
// author by kind (072 relay contributor, 071 person, 070 the App's own).
func TestClassifyStageMergeTrainDuplicateParksWithAuthor(t *testing.T) {
	reason := prstage.MergeTrain{State: prstage.MergeTrainStateDuplicateOf, Front: 15470, Position: 15469}.Reason()
	for _, tc := range []struct {
		name string
		lane string
		kind authorkind.Kind
		want stagelabels.Stage
	}{
		{"contributor clanker", stageLaneContributor, authorkind.Clanker, stagelabels.ReturnedToContributor},
		{"trusted human", stageLaneTrusted, authorkind.Human, stagelabels.NeedsAuthor},
		{"self", stageLaneSelf, authorkind.Hive, stagelabels.Fixing},
	} {
		v := ClassifyStage(stageObservation{Lane: tc.lane, AuthorKind: tc.kind, Reason: reason, HeadSHA: goldenHead})
		if v.Stage != tc.want || v.Rank != stageRankOwned || v.Human || v.Terminal {
			t.Fatalf("%s classified as %+v, want stage %v", tc.name, v, tc.want)
		}
	}
}

// hivecommons/hive#15825: items that have not passed the review or CI is
// not green should not be (a) ready for merge, (b) in a merge train. A
// train reason never lifts a head whose own evidence is red or whose
// reviewer asked for changes to 080; the admitted hold (green, reviewed,
// waiting for a seat) is 080.
func TestClassifyStageMergeTrainNotGreenOrReviewedIsNotMergeQueue(t *testing.T) {
	parked := prstage.MergeTrain{State: prstage.MergeTrainStateQueuedBehind, Front: 15748, Position: 3, Size: 4}.Reason()
	red := ClassifyStage(stageObservation{Lane: stageLaneSelf, Reason: parked, HeadSHA: goldenHead,
		Detail: stageDetail{RedChecks: []string{"coverage"}, RedChecksHead: goldenHead}})
	if red.Stage != stagelabels.Fixing || red.Reason != stageReasonOptionalCheckFailing+"coverage" {
		t.Fatalf("red check under a train reason classified as %+v, want 070 with the check named", red)
	}
	changes := ClassifyStage(stageObservation{Lane: stageLaneSelf, Reason: parked, HeadSHA: goldenHead,
		Detail: stageDetail{VerdictKnown: true, Verdict: prstage.ReviewVerdict{Verdict: "changes_requested"}}})
	if changes.Stage != stagelabels.Fixing || changes.Reason == "" {
		t.Fatalf("changes requested under a train reason classified as %+v, want 070", changes)
	}
	admitted := prstage.MergeTrain{State: prstage.MergeTrainStateAdmitted, Front: 15748, Position: 0, Size: 2}.Reason()
	v := ClassifyStage(stageObservation{Lane: stageLaneContributor, Reason: admitted, HeadSHA: goldenHead,
		Detail: stageDetail{VerdictKnown: true, Verdict: prstage.ReviewVerdict{Approved: true}}})
	if v.Stage != stagelabels.AwaitingMergeAdmission || v.Human {
		t.Fatalf("admitted candidate classified as %+v, want 080", v)
	}
}
