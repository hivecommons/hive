package main

import (
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/review"
)

type preparationReviewGitHub struct {
	observation automerge.BasePreparationObservation
	active      bool
	reads       int
}

func (f *preparationReviewGitHub) ActiveBasePreparation(context.Context, string, string, string) (automerge.BasePreparationObservation, bool, error) {
	f.reads++
	return f.observation, f.active, nil
}
func (*preparationReviewGitHub) ReviewPRChangedFiles(context.Context, string, int) ([]string, bool) {
	return nil, false
}
func (*preparationReviewGitHub) ReviewDeltaDiff(context.Context, string, string, string) review.DeltaDiff {
	return review.DeltaDiff{}
}
func (*preparationReviewGitHub) ReviewHeadsSamePatch(context.Context, string, string, string, string) bool {
	return false
}

func TestReviewPreparationHandoffWiringCurrentHeadAdmissionAndPerDestinationCache(t *testing.T) {
	for _, mode := range []string{"convoy", "no-owner", "policy-off", "pending-ci", "running-checks", "changed-head", "label-gated", "current-bundle", "stale-bundle"} {
		t.Run(mode, func(t *testing.T) {
			redirectReviewPaths(t)
			cfg := reviewSwarmConfig()
			cfg.Review.MaxParallelReviews = 1
			cfg.AutoMerge.RequireCurrentBase = []string{"hivecommons/hive"}
			owner := github.PullRequest{Repo: "hivecommons/hive", Number: 15868,
				Title: "fix: preparation owner", Author: "hive-bot", HeadSHA: reviewTestSHA,
				BaseRef: "v5", CIStatus: "success", Mergeable: github.MergeableYes, ReviewRank: 15}
			other := owner
			other.Number, other.HeadSHA, other.ReviewRank = 15819, strings.Repeat("b", 40), 1
			observer := &preparationReviewGitHub{
				active: true,
				observation: automerge.BasePreparationObservation{
					Repo: owner.Repo, BaseRef: owner.BaseRef, Number: owner.Number, HeadSHA: owner.HeadSHA,
				},
			}
			switch mode {
			case "no-owner":
				observer.active = false
			case "policy-off":
				cfg.AutoMerge.RequireCurrentBase = nil
				cfg.AutoMerge.SkipCurrentBase = []string{"hivecommons/hive"}
			case "pending-ci":
				owner.CIStatus, owner.CIChecksRunning = "pending", true
			case "running-checks":
				owner.CIChecksRunning = true
			case "changed-head":
				observer.observation.HeadSHA = strings.Repeat("c", 40)
			case "label-gated":
				cfg.Review.RequireLabels = []string{"review-allowed"}
				other.Labels = []string{"review-allowed"}
			case "current-bundle", "stale-bundle":
				head := owner.HeadSHA
				if mode == "stale-bundle" {
					head = strings.Repeat("d", 40)
				}
				if err := review.WriteArtifact("", review.Artifact{Items: []review.Aggregate{{
					Repo: owner.Repo, Number: owner.Number, HeadSHA: head, Verdict: review.VerdictApprove,
				}}}); err != nil {
					t.Fatal(err)
				}
			}
			actionable := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{other, owner}}}
			plan := planReviewDispatchWith(context.Background(), cfg, actionable, nil, nil, observer, restoreTestLogger())
			want := other.Number
			if mode == "convoy" || mode == "stale-bundle" {
				want = owner.Number
			}
			if len(plan.ReviewKicks) != 1 || plan.ReviewKicks[0].Number != want {
				t.Fatalf("mode=%s kicks=%+v want #%d", mode, plan.ReviewKicks, want)
			}
			if want == owner.Number && (plan.ReviewKicks[0].HeadSHA != owner.HeadSHA || plan.ReviewKicks[0].Priority != review.ReviewPriorityPreparation) {
				t.Fatalf("owner dispatch lost exact identity/priority: %+v", plan.ReviewKicks)
			}
			wantReads := 1
			if mode == "policy-off" {
				wantReads = 0
			}
			if observer.reads != wantReads {
				t.Fatalf("reads=%d want %d (one per destination)", observer.reads, wantReads)
			}
		})
	}
}
