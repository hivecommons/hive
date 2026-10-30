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

// ciReviewStages are the PR stages the tracker draws in the CI/Review
// column and its detours: the ones that must always have a non-human
// mover (CI, the review swarm, the clanker, the hive adopting, the author
// fixing) and never an operator decision (hivecommons/hive#15640, owner
// rule: "we dont want to create a log jam during pr in progress").
var ciReviewStages = map[stagelabels.Stage]bool{
	stagelabels.ReviewPending:         true,
	stagelabels.Reviewed:              true,
	stagelabels.Fixing:                true,
	stagelabels.NeedsAuthor:           true,
	stagelabels.ReturnedToContributor: true,
	stagelabels.AdoptedByHive:         true,
}

// securityHoldReasons are the lane reasons that are themselves a concrete
// security / trust finding and so may take a contributor PR out of
// hands-free CI/Review to 085.
var securityHoldReasons = map[string]bool{
	contributorLaneReasonSentinelBlocked:   true,
	contributorLaneReasonWorkflowPath:      true,
	contributorLaneReasonTestsRemoved:      true,
	contributorLaneReasonReporterTrustHold: true,
}

// operatorFaultReasons are not review decisions at all: the hive itself
// cannot run (no reviewer agent configured, App login unknown, the files
// API refused). Only the operator can fix the hive, so 085 is right.
var operatorFaultReasons = map[string]bool{
	contributorLaneReasonNoReviewer:    true,
	contributorLaneReasonNoAppBotLogin: true,
	contributorLaneReasonFilesEvidence: true,
}

// TestContributorLaneCIReviewNeverParksOnAHuman sweeps every contributor
// lane reason against the verdicts the aggregate can now produce for a
// contributor PR (none yet, approve, changes_requested, requires_human
// with a named finding) and asserts the invariant: a CI/Review stage is
// never human-owned, and 085 is reached only through a security lane
// reason, a verdict that names its finding, or an answered human request.
func TestContributorLaneCIReviewNeverParksOnAHuman(t *testing.T) {
	reasons := []string{
		contributorLaneReasonAppAuthored, contributorLaneReasonAuthorAssociation, contributorLaneReasonChangesRequested,
		contributorLaneReasonClaimAuthor, contributorLaneReasonClaimOtherHive, contributorLaneReasonClaimTier,
		contributorLaneReasonClaimUnverified, contributorLaneReasonFilesEvidence, contributorLaneReasonNoAppBotLogin,
		contributorLaneReasonNoAuthor, contributorLaneReasonNoClaim, contributorLaneReasonNoLinkedIssue,
		contributorLaneReasonNoReviewer, contributorLaneReasonNoVerdict, contributorLaneReasonReporterTrustHold,
		contributorLaneReasonSentinelBlocked, contributorLaneReasonSoaking + "(remaining=5m)", contributorLaneReasonTestsRemoved,
		contributorLaneReasonWorkflowPath, "check-pending", "check-failing:unit", "conflicting", "",
	}
	verdicts := map[string]stageDetail{
		"no verdict":        {},
		"approve":           {VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}},
		"changes_requested": {VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"}},
		"security finding":  {VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "requires_human", DecisionRequested: true, DecisionQuestion: "accept this workflow trigger?", DecisionFinding: "sensitive-paths question from the security review"}},
	}
	for _, reason := range reasons {
		for name, detail := range verdicts {
			v := ClassifyStage(stageObservation{Lane: stageLaneContributor, Reason: reason, HeadSHA: goldenHead, Detail: detail})
			if ciReviewStages[v.Stage] && v.Human {
				t.Errorf("reason %q verdict %s: CI/Review stage %s is human-owned (%+v) — a log jam", reason, name, v.Stage, v)
			}
			if v.Stage != stagelabels.NeedsDecision {
				continue
			}
			finding := detail.VerdictKnown && detail.Verdict.DecisionFinding != ""
			if !finding && !securityHoldReasons[reason] && !operatorFaultReasons[reason] {
				t.Errorf("reason %q verdict %s: 085 with no security finding and no security lane reason (%+v)", reason, name, v)
			}
		}
	}
}
