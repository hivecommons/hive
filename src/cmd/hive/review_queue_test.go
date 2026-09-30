package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

type fakePriorityLabeler struct {
	applied  []string
	removed  []string
	applyErr error
}

func (f *fakePriorityLabeler) ApplyHumanDecisionLabel(_ context.Context, repo string, number int, label string) error {
	f.applied = append(f.applied, github.ReviewQueueKey(repo, number)+" "+label)
	return f.applyErr
}

func (f *fakePriorityLabeler) RemoveLabel(_ context.Context, repo string, number int, label string) error {
	f.removed = append(f.removed, github.ReviewQueueKey(repo, number)+" "+label)
	return nil
}

func priorityQueue() []github.ReviewQueueEntry {
	return []github.ReviewQueueEntry{
		{Repo: "acme/a", Number: 1, Priority: github.ReviewPriorityHigh, Labels: []string{"review-priority/high"}},
		{Repo: "acme/a", Number: 2, Priority: github.ReviewPriorityNormal, Labels: []string{"review-priority/low"}},
		{Repo: "acme/a", Number: 3, Priority: github.ReviewPriorityLow},
	}
}

// Off by default: with priority_labels unset nothing is written, whatever
// the queue says.
func TestApplyReviewPriorityLabels_OffByDefault(t *testing.T) {
	f := &fakePriorityLabeler{}
	applyReviewPriorityLabels(context.Background(), &config.Config{}, f, priorityQueue(), discardLogger())
	applyReviewPriorityLabels(context.Background(), nil, f, priorityQueue(), discardLogger())
	on := &config.Config{Review: config.ReviewConfig{PriorityLabels: true}}
	applyReviewPriorityLabels(context.Background(), on, nil, priorityQueue(), discardLogger())
	applyReviewPriorityLabels(context.Background(), on, f, nil, discardLogger())
	if len(f.applied)+len(f.removed) != 0 {
		t.Fatalf("labels written while off: applied=%v removed=%v", f.applied, f.removed)
	}
}

func TestApplyReviewPriorityLabels_MirrorsRank(t *testing.T) {
	f := &fakePriorityLabeler{}
	cfg := &config.Config{Review: config.ReviewConfig{PriorityLabels: true}}
	applyReviewPriorityLabels(context.Background(), cfg, f, priorityQueue(), discardLogger())
	if want := []string{"acme/a#2 review-priority/normal", "acme/a#3 review-priority/low"}; !reflect.DeepEqual(f.applied, want) {
		t.Fatalf("applied = %v, want %v", f.applied, want)
	}
	if want := []string{"acme/a#2 review-priority/low"}; !reflect.DeepEqual(f.removed, want) {
		t.Fatalf("removed = %v, want %v", f.removed, want)
	}
}

// A label the repo does not have fails to apply; the stale one still goes,
// and the failure does not stop the next PR.
func TestApplyReviewPriorityLabels_ApplyFailureStillRemovesStale(t *testing.T) {
	f := &fakePriorityLabeler{applyErr: errors.New("label not present")}
	cfg := &config.Config{Review: config.ReviewConfig{PriorityLabels: true}}
	applyReviewPriorityLabels(context.Background(), cfg, f, priorityQueue(), discardLogger())
	if len(f.applied) != 2 || len(f.removed) != 1 {
		t.Fatalf("applied=%v removed=%v", f.applied, f.removed)
	}
}

func TestStampReviewQueue_UsesVerdictArtifact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "review-verdicts.json")
	orig := review.ReviewVerdictsPath
	review.ReviewVerdictsPath = path
	t.Cleanup(func() { review.ReviewVerdictsPath = orig })

	cfg := &config.Config{Project: config.ProjectConfig{Org: "acme"}}
	newActionable := func() *github.ActionableResult {
		return &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{
			{Repo: "a", Number: 1, Title: "docs: x", HeadSHA: "h1", CreatedAt: time.Now().Add(-time.Hour)},
		}}}
	}

	if stampReviewQueue(nil, newActionable(), discardLogger()) != nil || stampReviewQueue(cfg, nil, discardLogger()) != nil {
		t.Fatal("nil inputs must stamp nothing")
	}

	// No artifact yet: ranked, stamped, unreviewed.
	actionable := newActionable()
	q := stampReviewQueue(cfg, actionable, discardLogger())
	if len(q) != 1 || q[0].Reviewed || actionable.PRs.Items[0].ReviewRank != 1 {
		t.Fatalf("missing artifact: queue=%+v stamp=%d", q, actionable.PRs.Items[0].ReviewRank)
	}

	// Unreadable artifact: same, with a warning rather than a failure.
	if err := os.WriteFile(path, []byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	actionable = newActionable()
	if q := stampReviewQueue(cfg, actionable, discardLogger()); len(q) != 1 || q[0].Reviewed {
		t.Fatalf("unreadable artifact: queue=%+v", q)
	}

	// A verdict for the current head, recorded under the full repo name.
	if err := review.WriteArtifact(path, review.Artifact{Items: []review.Aggregate{{
		Repo: "acme/a", Number: 1, HeadSHA: "h1", Verdict: review.VerdictApprove,
		Confidence: review.Confidence{Score: review.ConfidenceSafeFloor},
	}}}); err != nil {
		t.Fatalf("WriteArtifact: %v", err)
	}
	actionable = newActionable()
	q = stampReviewQueue(cfg, actionable, discardLogger())
	if len(q) != 1 || !q[0].Reviewed || q[0].ConfidenceBand != github.ReviewQueueBandSafe {
		t.Fatalf("with verdict: queue=%+v", q)
	}
	if got := actionable.PRs.Items[0].ReviewRankReasons; len(got) == 0 {
		t.Fatal("reasons not stamped onto the snapshot")
	}
}
