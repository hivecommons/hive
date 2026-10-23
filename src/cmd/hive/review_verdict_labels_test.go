package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

// The governor mirrors each PR head's review state onto one verdict label:
// in progress while dispatched, then the settled verdict (#13833).
func TestApplyReviewVerdictLabels(t *testing.T) {
	dir := t.TempDir()
	oldPath, oldLegacy := review.ReviewVerdictsPath, review.LegacyReviewVerdictsPath
	review.ReviewVerdictsPath = filepath.Join(dir, review.ReviewVerdictsFile)
	review.LegacyReviewVerdictsPath = filepath.Join(dir, "legacy-"+review.ReviewVerdictsFile)
	t.Cleanup(func() { review.ReviewVerdictsPath, review.LegacyReviewVerdictsPath = oldPath, oldLegacy })
	if err := review.WriteArtifact("", review.Artifact{Items: []review.Aggregate{
		{Repo: "acme/a", Number: 1, HeadSHA: "h1", Verdict: review.VerdictApprove},
	}}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Project.Org = "acme"
	cfg.Review.VerdictLabels = config.ReviewVerdictLabelsConfig{
		Reviewing: "hive-review/in-progress",
		Approve:   "hive-review/approved",
	}
	actionable := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{
		{Repo: "acme/a", Number: 1, HeadSHA: "h1", Labels: []string{"hive-review/in-progress"}},
		{Repo: "acme/a", Number: 2, HeadSHA: "h2"},
		{Repo: "acme/a", Number: 3, HeadSHA: "h3", Labels: []string{"hive-review/approved"}},
	}}}
	plan := review.DispatchPlan{State: review.DispatchState{Pending: []review.PendingReview{
		{Repo: "acme/a", Number: 2, HeadSHA: "h2", Perspective: review.PerspectiveCorrectness, Agent: "r"},
	}}}
	labeler := &fakePriorityLabeler{}
	applyReviewVerdictLabels(context.Background(), cfg, labeler, actionable, plan, nil)
	if got := strings.Join(labeler.applied, "|"); got != "acme/a#1 hive-review/approved|acme/a#2 hive-review/in-progress" {
		t.Fatalf("applied = %q", got)
	}
	if got := strings.Join(labeler.removed, "|"); got != "acme/a#1 hive-review/in-progress|acme/a#3 hive-review/approved" {
		t.Fatalf("removed = %q", got)
	}

	// Off: nothing is touched.
	cfg.Review.VerdictLabels = config.ReviewVerdictLabelsConfig{}
	off := &fakePriorityLabeler{}
	applyReviewVerdictLabels(context.Background(), cfg, off, actionable, plan, nil)
	if len(off.applied)+len(off.removed) != 0 {
		t.Fatalf("disabled feature wrote labels: %+v", off)
	}
}
