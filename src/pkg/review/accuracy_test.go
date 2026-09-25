package review

import (
	"testing"
	"time"
)

func TestSummarizeReviewerAccuracyByPerspectiveAndModel(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	ledger := &OutcomeLedger{Items: map[string]*PROutcome{
		"acme/a#1": {Repo: "acme/a", Number: 1, FirstSeenAt: now.Add(-2 * time.Hour), Outcome: OutcomeMerged, OutcomeAt: now.Add(-time.Hour)},
		"acme/a#2": {Repo: "acme/a", Number: 2, FirstSeenAt: now.Add(-2 * time.Hour), Outcome: OutcomeMerged, OutcomeAt: now.Add(-time.Hour)},
		"acme/a#3": {Repo: "acme/a", Number: 3, FirstSeenAt: now.Add(-2 * time.Hour), Outcome: OutcomeClosed, OutcomeAt: now.Add(-time.Hour)},
	}}
	artifact := Artifact{Items: []Aggregate{
		{
			Repo: "acme/a", Number: 1, HeadSHA: "a", ReviewModel: "reviewer-a", Verdict: VerdictApprove,
			Perspectives: map[Perspective]Verdict{PerspectiveCorrectness: VerdictApprove}, Confidence: Confidence{Score: 5}, RecordedAt: now.Add(-time.Hour),
		},
		{
			Repo: "acme/a", Number: 2, HeadSHA: "b", ReviewModel: "reviewer-a", Verdict: VerdictChangesRequested,
			Perspectives: map[Perspective]Verdict{PerspectiveCorrectness: VerdictChangesRequested, PerspectiveSecurity: VerdictApprove}, Confidence: Confidence{Score: 2}, RecordedAt: now.Add(-time.Hour),
		},
		{
			Repo: "acme/a", Number: 3, HeadSHA: "c", ReviewModel: "reviewer-b", Verdict: VerdictApprove,
			Perspectives: map[Perspective]Verdict{PerspectiveSecurity: VerdictApprove}, Confidence: Confidence{Score: 4}, RecordedAt: now.Add(-time.Hour),
		},
		{
			Repo: "acme/a", Number: 4, HeadSHA: "old", ReviewModel: "reviewer-b", Verdict: VerdictApprove,
			Perspectives: map[Perspective]Verdict{PerspectiveSecurity: VerdictApprove}, Confidence: Confidence{Score: 5}, RecordedAt: now.Add(-60 * 24 * time.Hour),
		},
	}}
	sum := SummarizeReviewerAccuracy(now, 30*24*time.Hour, artifact, ledger, map[string]ReviewerAccuracyEvidence{
		"acme/a#1": {ReworkedAfterApproval: true},
		"acme/a#2": {MergedUnchangedAfterBlock: true},
	})
	if sum.Samples != 3 || sum.WindowDays != 30 {
		t.Fatalf("summary counts = %+v", sum)
	}
	correctness := findAccuracyGroup(t, sum.Perspectives, "correctness")
	if correctness.Approvals != 1 || correctness.FalseApprovals != 1 || correctness.Blocks != 1 || correctness.FalseBlocks != 1 {
		t.Fatalf("correctness accuracy = %+v", correctness)
	}
	if correctness.FalseApproveRate != 1 || correctness.FalseBlockRate != 1 {
		t.Fatalf("correctness rates = %+v", correctness)
	}
	model := findAccuracyGroup(t, sum.ReviewerModels, "reviewer-a")
	if model.Samples != 2 || model.FalseApprovals != 1 || model.FalseBlocks != 1 {
		t.Fatalf("reviewer-a accuracy = %+v", model)
	}
	if got := findCalibrationBucket(t, model.ConfidenceBuckets, "5"); got.Merged != 1 || got.BadOutcomes != 1 || got.MergeRate != 1 || got.BadOutcomeRate != 1 {
		t.Fatalf("confidence bucket 5 = %+v", got)
	}
}

func findAccuracyGroup(t *testing.T, groups []ReviewerAccuracyGroup, name string) ReviewerAccuracyGroup {
	t.Helper()
	for _, g := range groups {
		if g.Name == name {
			return g
		}
	}
	t.Fatalf("missing group %q in %+v", name, groups)
	return ReviewerAccuracyGroup{}
}

func findCalibrationBucket(t *testing.T, buckets []ReviewerConfidenceCalibrationBucket, name string) ReviewerConfidenceCalibrationBucket {
	t.Helper()
	for _, b := range buckets {
		if b.Bucket == name {
			return b
		}
	}
	t.Fatalf("missing bucket %q in %+v", name, buckets)
	return ReviewerConfidenceCalibrationBucket{}
}
