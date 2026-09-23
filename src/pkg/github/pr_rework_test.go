package github

import (
	"testing"
	"time"
)

func TestBuildPRReworkStatsFromReviewAndCommitTimeline(t *testing.T) {
	created := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	firstReview := created.Add(1 * time.Hour)
	merged := created.Add(5 * time.Hour)
	pr := PullRequest{CreatedAt: created, MergedAt: merged}
	reviews := []prReviewEvent{
		{State: "COMMENTED", CommitID: "a", SubmittedAt: firstReview.Add(-10 * time.Minute), Author: "hive[bot]", AuthorType: "Bot"},
		{State: "CHANGES_REQUESTED", CommitID: "a", SubmittedAt: firstReview, Author: "hive[bot]", AuthorType: "Bot"},
		{State: "CHANGES_REQUESTED", CommitID: "a", SubmittedAt: firstReview.Add(5 * time.Minute), Author: "hive[bot]", AuthorType: "Bot"},
		{State: "CHANGES_REQUESTED", CommitID: "b", SubmittedAt: firstReview.Add(2 * time.Hour), Author: "alice", AuthorType: "User"},
	}
	commits := []PRCommit{
		{Message: "initial", AuthoredAt: created.Add(30 * time.Minute)},
		{Message: "fix review\n\nAuto-fix attempt 2/6\n— hive: agent=scanner backend=copilot model=claude-opus-5", AuthoredAt: firstReview.Add(30 * time.Minute)},
		{Message: "polish", AuthoredAt: firstReview.Add(90 * time.Minute)},
	}
	got := BuildPRReworkStats(pr, reviews, commits)
	if got.ReviewRounds != 1 || got.FixAttempts != 2 || got.HumanChangeRequests != 1 || got.FollowUpCommits != 2 {
		t.Fatalf("stats = %+v, want 1 hive round, 2 attempts, 1 human request, 2 follow-up commits", got)
	}
	if got.FirstPass {
		t.Fatal("reworked PR must not be first-pass")
	}
	if got.TimeToMergeMinutes != 300 {
		t.Fatalf("time to merge = %d, want 300", got.TimeToMergeMinutes)
	}
	if len(got.FixerModels) != 1 || got.FixerModels[0] != "claude-opus-5" {
		t.Fatalf("fixer models = %#v, want claude-opus-5", got.FixerModels)
	}
	if len(got.FixerBackends) != 1 || got.FixerBackends[0] != "copilot" {
		t.Fatalf("fixer backends = %#v, want copilot", got.FixerBackends)
	}
}

func TestBuildPRReworkStatsFirstPass(t *testing.T) {
	created := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	got := BuildPRReworkStats(PullRequest{CreatedAt: created, MergedAt: created.Add(time.Hour)}, nil, nil)
	if !got.FirstPass || got.ReviewRounds != 0 || got.FixAttempts != 0 {
		t.Fatalf("first-pass stats = %+v", got)
	}
}
