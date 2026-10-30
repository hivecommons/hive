package stageengine

import (
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Owner rule (2026-10-29): "there should be no decisions by a human while
// these are in ci/review — unless the review found something." Twelve of
// hive's own contributor-lane PRs (#15312, #15324, #15410, …) sat 3–21h as
// 085 records the tracker read as "PR in progress". The aggregate now
// resolves a decision question with nothing behind it as advisory, so the
// stage engine only ever sees a decision-requested verdict that names its
// finding; this pins the three shapes the lane must produce.
func TestClassifyStageContributorLaneNoDecisionDuringCIReview(t *testing.T) {
	cases := []struct {
		name       string
		reason     string
		detail     stageDetail
		wantStage  stagelabels.Stage
		wantReason string
	}{
		{
			// (a) no verdict yet: the head waits for the swarm, nobody is asked.
			name:      "no verdict → review pending, not 085",
			reason:    contributorLaneReasonNoVerdict,
			detail:    stageDetail{},
			wantStage: stagelabels.ReviewPending,
		},
		{
			name:      "no verdict with CI pending → review pending, not 085",
			reason:    "check-pending",
			detail:    stageDetail{},
			wantStage: stagelabels.ReviewPending,
		},
		{
			// (b) the review found something: 085 with the finding carried.
			name:       "decision backed by a finding → 085 with the decision-requested reason",
			reason:     "check-pending",
			detail:     stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "requires_human", DecisionRequested: true, DecisionQuestion: "close #15312 in favour of #15309?", DecisionFinding: "duplicate: the review found this change already landed or open elsewhere"}},
			wantStage:  stagelabels.NeedsDecision,
			wantReason: StageReasonDecisionRequested,
		},
		{
			// (c) green and approved: the merge gate's human-merge state.
			name:      "green + approved, author below the lane's trust → 090 ready for human merge, not 085",
			reason:    contributorLaneReasonClaimTier,
			detail:    stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}},
			wantStage: stagelabels.ReadyForHumanMerge,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := ClassifyStage(stageObservation{Lane: stageLaneContributor, Reason: tc.reason, HeadSHA: goldenHead, Detail: tc.detail})
			if v.Stage != tc.wantStage {
				t.Fatalf("classified as %+v, want stage %s", v, tc.wantStage)
			}
			if tc.wantReason != "" && v.Reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", v.Reason, tc.wantReason)
			}
			if tc.wantStage != stagelabels.NeedsDecision && v.Stage == stagelabels.NeedsDecision {
				t.Fatalf("classified as %+v, want not needs-decision", v)
			}
		})
	}
}
