package github

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/review"
)

func TestBuildPRReworkStatsFromReviewAndCommitTimeline(t *testing.T) {
	isolateReworkState(t)
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
	isolateReworkState(t)
	created := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	got := BuildPRReworkStats(PullRequest{CreatedAt: created, MergedAt: created.Add(time.Hour)}, nil, nil)
	if !got.FirstPass || got.ReviewRounds != 0 || got.FixAttempts != 0 {
		t.Fatalf("first-pass stats = %+v", got)
	}
}

func TestBuildPRReworkStatsCountsCommentedHiveVerdict(t *testing.T) {
	isolateReworkState(t)
	created := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	reviewed := created.Add(30 * time.Minute)
	got := BuildPRReworkStats(
		PullRequest{CreatedAt: created, MergedAt: created.Add(time.Hour)},
		[]prReviewEvent{{
			State:       "COMMENTED",
			Author:      "hivecommons-hive[bot]",
			AuthorType:  "Bot",
			CommitID:    "head-a",
			SubmittedAt: reviewed,
			Body:        `{"verdict":"changes_requested","summary":"blocking finding"}`,
		}},
		nil,
	)
	if got.ReviewRounds != 1 || got.FirstReviewAt != reviewed || got.FirstPass {
		t.Fatalf("commented hive verdict stats = %+v, want one rework round and not first-pass", got)
	}
}

func TestBuildPRReworkStatsDanathar6358Fixture(t *testing.T) {
	isolateReworkState(t)
	created := time.Date(2026, 9, 20, 12, 30, 0, 0, time.UTC)
	reviewed := time.Date(2026, 9, 20, 13, 33, 0, 0, time.UTC)
	merged := time.Date(2026, 9, 20, 14, 25, 0, 0, time.UTC)
	got := BuildPRReworkStatsWithComments(
		PullRequest{Repo: "hivecommons/hive", Number: 6358, Author: "hivecommons-hive[bot]", CreatedAt: created, MergedAt: merged},
		nil,
		[]prCommentEvent{{
			Author:     "clubanderson",
			AuthorType: "User",
			CreatedAt:  reviewed,
			Body:       "One blocking CI failure, caused by this PR (not a flake): TestSleepRatchet",
		}},
		[]PRCommit{
			{SHA: "3f37205", Author: "hivecommons-hive[bot]", Message: "initial model work", AuthoredAt: time.Date(2026, 9, 20, 12, 37, 0, 0, time.UTC)},
			{SHA: "138ffca", Author: "clubanderson", Message: "poll goroutine settle via testutil.Eventually, not time.Sleep", AuthoredAt: time.Date(2026, 9, 20, 13, 57, 0, 0, time.UTC)},
		},
	)
	if got.FirstPass || got.FollowUpCommits != 1 || got.FixAttempts != 1 || !got.FirstReviewAt.Equal(reviewed) {
		t.Fatalf("Danathar #6358 fixture stats = %+v, want reworked with one follow-up/fix attempt", got)
	}
}

func TestBuildPRReworkStatsCountsDispatchAttemptState(t *testing.T) {
	statePath := isolateReworkState(t)
	dispatched := time.Date(2026, 9, 20, 11, 0, 0, 0, time.UTC)
	if err := review.WriteDispatchState(statePath, review.DispatchState{
		FixAttempts: []review.FixAttempt{{
			Repo:       "hivecommons/hive",
			Number:     8772,
			HeadSHA:    "head-a",
			Agent:      "scanner",
			Attempts:   2,
			Dispatched: dispatched,
		}},
	}); err != nil {
		t.Fatalf("write dispatch state: %v", err)
	}
	created := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	got := BuildPRReworkStats(PullRequest{Repo: "hivecommons/hive", Number: 8772, CreatedAt: created, MergedAt: created.Add(time.Hour)}, nil, nil)
	if got.FixAttempts != 2 || got.FirstPass {
		t.Fatalf("dispatch-attempt stats = %+v, want two fix attempts and not first-pass", got)
	}
}

func TestPRReworkCacheSchemaInvalidatesOldRows(t *testing.T) {
	isolateReworkState(t)
	PRReworkCachePath = filepath.Join(t.TempDir(), PRReworkCacheFile)
	old := prReworkCacheFile{
		GeneratedAt: time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC),
		Items:       map[string]PRReworkStats{"hivecommons/hive#6358": {FirstPass: true}},
	}
	data, err := json.Marshal(old)
	if err != nil {
		t.Fatalf("marshal old cache: %v", err)
	}
	if err := os.WriteFile(PRReworkCachePath, data, 0o644); err != nil {
		t.Fatalf("write old cache: %v", err)
	}
	if got := loadPRReworkCache(); len(got) != 0 {
		t.Fatalf("old schema cache loaded = %+v, want empty", got)
	}
	storePRReworkCache(map[string]PRReworkStats{"hivecommons/hive#6358": {FollowUpCommits: 1}})
	if got := loadPRReworkCache(); got["hivecommons/hive#6358"].FollowUpCommits != 1 {
		t.Fatalf("new schema cache did not round-trip: %+v", got)
	}
}

func isolateReworkState(t *testing.T) string {
	t.Helper()
	oldDispatchPath := review.ReviewDispatchStatePath
	oldLegacyPath := review.LegacyReviewDispatchStatePath
	oldCachePath := PRReworkCachePath
	root := t.TempDir()
	dispatchPath := filepath.Join(root, review.ReviewDispatchStateFile)
	review.ReviewDispatchStatePath = dispatchPath
	review.LegacyReviewDispatchStatePath = filepath.Join(root, "legacy-"+review.ReviewDispatchStateFile)
	PRReworkCachePath = filepath.Join(root, PRReworkCacheFile)
	t.Cleanup(func() {
		review.ReviewDispatchStatePath = oldDispatchPath
		review.LegacyReviewDispatchStatePath = oldLegacyPath
		PRReworkCachePath = oldCachePath
	})
	return dispatchPath
}
