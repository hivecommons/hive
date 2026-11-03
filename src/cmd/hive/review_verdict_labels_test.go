package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// A settled verdict on a PR still showing in-progress takes in-progress off
// before approved goes on, journaled against the PR state the plan read
// (hivecommons/hive#14361: #2315 showed both after its in-progress label had
// been removed once before, because the plain removal deduped forever).
func TestApplyReviewVerdictLabelsSwapsStateAsOfThePRState(t *testing.T) {
	dir := t.TempDir()
	oldPath, oldLegacy := review.ReviewVerdictsPath, review.LegacyReviewVerdictsPath
	review.ReviewVerdictsPath = filepath.Join(dir, review.ReviewVerdictsFile)
	review.LegacyReviewVerdictsPath = filepath.Join(dir, "legacy-"+review.ReviewVerdictsFile)
	t.Cleanup(func() { review.ReviewVerdictsPath, review.LegacyReviewVerdictsPath = oldPath, oldLegacy })
	if err := review.WriteArtifact("", review.Artifact{Items: []review.Aggregate{
		{Repo: "acme/a", Number: 2315, HeadSHA: "h1", Verdict: review.VerdictApprove},
	}}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Project.Org = "acme"
	cfg.Review.VerdictLabels = config.ReviewVerdictLabelsConfig{
		Reviewing: "hive-review/in-progress",
		Approve:   "hive-review/approved",
	}
	updated := time.Date(2026, 10, 26, 14, 21, 0, 0, time.UTC)
	actionable := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{
		{Repo: "acme/a", Number: 2315, HeadSHA: "h1", Labels: []string{"hive-review/in-progress"}, UpdatedAt: updated},
	}}}
	labeler := &fakePriorityLabeler{}

	applyReviewVerdictLabels(context.Background(), cfg, labeler, actionable, review.DispatchPlan{}, nil)

	if got := strings.Join(labeler.ops, "|"); got != "-acme/a#2315 hive-review/in-progress|+acme/a#2315 hive-review/approved" {
		t.Fatalf("ops = %q, want the stale state removed, then the new one applied", got)
	}
	if len(labeler.asOf) != 1 || !labeler.asOf[0].Equal(updated) {
		t.Fatalf("removal journaled as of %v, want the PR's updated_at %v", labeler.asOf, updated)
	}
}

func resetVerdictLabelSweep() {
	verdictLabelSweep.mu.Lock()
	verdictLabelSweep.last = time.Time{}
	verdictLabelSweep.mu.Unlock()
}

// A PR that merged or closed while carrying a verdict label loses it: the
// open-PR mirror never sees it again (hivecommons/hive#14361: #2331 merged
// still showing in-progress). Only governed repos are touched, the sweep is
// throttled, and one run removes at most reviewVerdictLabelMaxChanges.
func TestSweepClosedPRVerdictLabels(t *testing.T) {
	resetVerdictLabelSweep()
	t.Cleanup(resetVerdictLabelSweep)
	cfg := &config.Config{}
	cfg.Project.Org = "acme"
	cfg.Project.Repos = []string{"a"}
	cfg.Review.VerdictLabels = config.ReviewVerdictLabelsConfig{
		Reviewing: "hive-review/in-progress",
		Approve:   "hive-review/approved",
	}
	merged := time.Date(2026, 10, 26, 15, 12, 0, 0, time.UTC)
	labeler := &fakePriorityLabeler{closed: map[string][]github.LabelledPR{
		"hive-review/in-progress": {
			{Repo: "acme/a", Number: 2331, UpdatedAt: merged},
			{Repo: "acme/other", Number: 9, UpdatedAt: merged},
		},
		"hive-review/approved": {{Repo: "ACME/a", Number: 2320, UpdatedAt: merged}},
	}}
	now := time.Date(2026, 10, 26, 16, 0, 0, 0, time.UTC)

	sweepClosedPRVerdictLabels(context.Background(), cfg, labeler, now, nil)

	if got := strings.Join(labeler.removed, "|"); got != "acme/a#2331 hive-review/in-progress|acme/a#2320 hive-review/approved" {
		t.Fatalf("removed = %q, want only the governed repo's closed PRs", got)
	}
	if len(labeler.applied) != 0 {
		t.Fatalf("sweep applied labels: %v", labeler.applied)
	}
	for _, at := range labeler.asOf {
		if !at.Equal(merged) {
			t.Fatalf("removal journaled as of %v, want the closed PR's updated_at", at)
		}
	}

	// Throttled: a second run inside the interval searches nothing.
	searches := labeler.searches
	sweepClosedPRVerdictLabels(context.Background(), cfg, labeler, now.Add(time.Minute), nil)
	if labeler.searches != searches {
		t.Fatalf("sweep ran again inside its interval (%d searches, was %d)", labeler.searches, searches)
	}

	// Bounded: one run removes at most reviewVerdictLabelMaxChanges.
	var many []github.LabelledPR
	for i := 1; i <= reviewVerdictLabelMaxChanges+10; i++ {
		many = append(many, github.LabelledPR{Repo: "acme/a", Number: 1000 + i, UpdatedAt: merged})
	}
	bounded := &fakePriorityLabeler{closed: map[string][]github.LabelledPR{
		"hive-review/in-progress": many,
		"hive-review/approved":    many,
	}}
	sweepClosedPRVerdictLabels(context.Background(), cfg, bounded, now.Add(reviewVerdictLabelSweepInterval), nil)
	if len(bounded.removed) != reviewVerdictLabelMaxChanges {
		t.Fatalf("one sweep removed %d labels, want at most %d", len(bounded.removed), reviewVerdictLabelMaxChanges)
	}

	// Off: nothing is searched.
	resetVerdictLabelSweep()
	cfg.Review.VerdictLabels = config.ReviewVerdictLabelsConfig{}
	off := &fakePriorityLabeler{}
	sweepClosedPRVerdictLabels(context.Background(), cfg, off, now, nil)
	if off.searches != 0 || len(off.removed) != 0 {
		t.Fatalf("disabled feature swept: %+v", off)
	}
}
