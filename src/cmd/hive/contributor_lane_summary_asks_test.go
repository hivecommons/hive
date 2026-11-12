package main

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

// A changes_requested with no structured finding still tells the author
// something: the lane verdict carries what the blocking reviewers said in
// their summaries, so the 072 nudge never reads as the lead line alone.
func TestContributorLaneVerdictCarriesBlockingReviewerSummaries(t *testing.T) {
	now := time.Now().UTC()
	agg := review.Aggregate{
		Repo: "acme/widgets", Number: 7, HeadSHA: "sha1", Verdict: review.VerdictChangesRequested, RecordedAt: now,
		Perspectives:   map[review.Perspective]review.Verdict{review.PerspectiveCorrectness: review.VerdictChangesRequested},
		BlockSummaries: []string{"correctness: the early return in the handler is still present at this head"},
	}
	v := contributorLaneVerdictFromArtifact(review.Artifact{Items: []review.Aggregate{agg}}, "acme/widgets", 7, "sha1", false)
	if !stageengine.VerdictBlocks(prstage.Detail{Verdict: v, VerdictKnown: true}) {
		t.Fatalf("verdict = %+v, want a blocking changes_requested", v)
	}
	if len(v.FindingAsks) != 0 {
		t.Fatalf("finding asks = %q, want none (the aggregate has no finding)", v.FindingAsks)
	}
	if want := []string{"correctness: the early return in the handler is still present at this head"}; !slices.Equal(v.SummaryAsks, want) {
		t.Fatalf("summary asks = %q, want %q", v.SummaryAsks, want)
	}

	// No verdict for the head: nothing to carry.
	if v := contributorLaneVerdictFromArtifact(review.Artifact{Items: []review.Aggregate{agg}}, "acme/widgets", 7, "sha2", false); len(v.SummaryAsks) != 0 || len(v.FindingAsks) != 0 {
		t.Fatalf("another head's verdict carried asks: %+v", v)
	}
}

// More findings than the nudge lists: the lane verdict carries the first
// contributorNudgeFindingLimit and one line counting the rest. Reviewer
// summaries share the cap.
func TestContributorLaneVerdictCapsTheAsksTheNudgeLists(t *testing.T) {
	const extra = 2
	agg := review.Aggregate{
		Repo: "acme/widgets", Number: 7, HeadSHA: "sha1", Verdict: review.VerdictChangesRequested, RecordedAt: time.Now().UTC(),
		Perspectives: map[review.Perspective]review.Verdict{review.PerspectiveCorrectness: review.VerdictChangesRequested},
	}
	for i := 1; i <= contributorNudgeFindingLimit+extra; i++ {
		agg.Findings = append(agg.Findings, review.PerspectiveFinding{Perspective: review.PerspectiveCorrectness, Finding: outputschema.Finding{
			Title: fmt.Sprintf("finding %d", i), Severity: outputschema.SeverityMedium, File: "src/widget.go", Line: i,
		}})
		agg.BlockSummaries = append(agg.BlockSummaries, fmt.Sprintf("perspective-%d: summary %d", i, i))
	}
	v := contributorLaneVerdictFromArtifact(review.Artifact{Items: []review.Aggregate{agg}}, "acme/widgets", 7, "sha1", false)
	if len(v.FindingAsks) != contributorNudgeFindingLimit+1 {
		t.Fatalf("finding asks = %q, want %d and one counting line", v.FindingAsks, contributorNudgeFindingLimit)
	}
	if v.FindingAsks[0] != "`src/widget.go:1` finding 1" || v.FindingAsks[contributorNudgeFindingLimit] != "and 2 more finding(s) in the review" {
		t.Fatalf("finding asks = %q", v.FindingAsks)
	}
	if len(v.SummaryAsks) != contributorNudgeFindingLimit+1 || v.SummaryAsks[contributorNudgeFindingLimit] != "and 2 more reviewer summary line(s) in the review" {
		t.Fatalf("summary asks = %q, want %d and one counting line", v.SummaryAsks, contributorNudgeFindingLimit)
	}
}
