package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

// A head whose verdict a delta re-stamp carried from an earlier head has no
// reviewer and nothing published, so its evidence bundle holds no verdicts.
// The Review track used to fall through to "not started · hive reviewer
// next" while the perspective count read 5/5 from the carried aggregate.
// It now reads as the carried verdict, naming the head it came from.
func TestStageTrackerReviewActivityCarriedVerdictIsNotNotStarted(t *testing.T) {
	now := time.Date(2026, 11, 12, 11, 0, 0, 0, time.UTC)
	pr := github.PullRequest{Repo: "acme/widgets", Number: 7, HeadSHA: "35ebb52aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	perspectives := map[review.Perspective]review.Verdict{}
	for _, p := range review.DefaultPerspectives {
		perspectives[p] = review.VerdictApprove
	}
	perspectives[review.DefaultPerspectives[0]] = review.VerdictChangesRequested
	agg := &review.Aggregate{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Verdict: review.VerdictChangesRequested,
		RecordedAt: now, Perspectives: perspectives,
		Delta: &review.DeltaReviewInfo{PreviousHead: "26fe096bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Trivial: true, Note: "re-stamped from 26fe096bbbbb: identical tree"}}
	bundle := &evidence.Bundle{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, UpdatedAt: now}
	ci := &StageTrackerActivity{Head: pr.HeadSHA, State: "passing", ChecksPassed: 3, ChecksTotal: 3}

	for name, b := range map[string]*evidence.Bundle{"empty bundle": bundle, "no bundle": nil} {
		t.Run(name, func(t *testing.T) {
			got := stageTrackerReviewActivity(pr, b, agg, review.DispatchState{}, nil, now)
			stageTrackerCountPerspectives(got, pr, b, agg, review.DispatchState{}, stageTrackerRepoQualifier(nil), review.DefaultPerspectives)
			stageTrackerReviewFallbackState(got, stageTrackerReviewFallbackInputs{Head: pr.HeadSHA, CI: ci, ReviewerLane: true})
			if got.State != "changes_requested" || got.State == reviewStateNotStarted {
				t.Fatalf("state = %s (%q), want the carried changes_requested", got.State, got.Caption)
			}
			if !strings.Contains(got.Caption, "carried from 26fe096") || !strings.Contains(got.Detail, "No reviewer ran on this commit") || !strings.Contains(got.Source, "delta-restamp") {
				t.Fatalf("carried track = %+v", got)
			}
			if got.PerspectivesDone != got.PerspectivesTotal || got.PerspectivesDone == 0 {
				t.Fatalf("perspectives %d/%d", got.PerspectivesDone, got.PerspectivesTotal)
			}
		})
	}

	// A carried approve (base-sync) reads approved, with the same note.
	approve := *agg
	approve.Verdict, approve.MergeEligible = review.VerdictApprove, true
	approve.Delta = &review.DeltaReviewInfo{PreviousHead: "26fe096bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Trivial: true, BaseSync: true}
	if got := stageTrackerReviewActivity(pr, nil, &approve, review.DispatchState{}, nil, now); got.State != "approved" || !strings.Contains(got.Caption, "carried from 26fe096") {
		t.Fatalf("carried approve = %+v", got)
	}

	// A head the swarm reviewed itself is untouched by this path.
	reviewed := *agg
	reviewed.Delta = nil
	if got := stageTrackerReviewActivity(pr, nil, &reviewed, review.DispatchState{}, nil, now); got.State != "unknown" {
		t.Fatalf("reviewed head without a bundle changed: %+v", got)
	}
}
