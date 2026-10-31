package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestStageTrackerEarlyMergeRefusalDoesNotAdmitPendingHead(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	attempt := github.MergeAttempt{Repo: "hivecommons/hive", Number: 16014, Head: "current-head",
		StartedAt: now.Add(-time.Minute), EndedAt: now,
		Error: "405 1 of 4 required status checks are expected."}
	for _, stage := range []stagelabels.Stage{stagelabels.ReviewPending, stagelabels.Reviewed, stagelabels.MergeQueue} {
		rec := stagelabels.Record{Stage: stage, Reason: "base-refresh:queued-behind#16007"}
		mg := stageTrackerMergeGateAttempt(stageTrackerMergeGate(attempt.Repo, attempt.Number, stage, rec, true,
			automerge.SweepState{}, now), attempt, true, attempt.Head, stage, rec, true)
		if stage == stagelabels.MergeQueue {
			if mg == nil || mg.State != MergeGateStateMergeFailed || mg.MergeFailedKind != MergeRefusalChecksPending {
				t.Fatalf("actual queue outcome was demoted: %+v", mg)
			}
			continue
		}
		if mg == nil || mg.State == MergeGateStateMergeFailed || mg.PriorHeadMergeFailedKind != MergeRefusalChecksPending {
			t.Fatalf("early required-check refusal fabricated queue admission: %+v", mg)
		}
		it := StageTrackerItem{Kind: "pr", Stage: stage.Number(), MergeGate: mg,
			Preparation: &StageTrackerPreparation{Role: "waiting", State: "current", Number: attempt.Number, Behind: 16007}}
		if stageTrackerItemColumn(&it) != trackerColumnReview || it.MergeTrain != nil {
			t.Fatalf("a pending head's preparation ticket became a ready train seat: %+v", it)
		}
	}
}
