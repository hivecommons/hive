package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func TestAggregateGovernorPRModelsWindowsAndOutcomes(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	prs := []ghpkg.PullRequest{
		{HiveAttributed: true, HiveModel: "Claude-Fable-5", HiveBackend: "Claude", HiveAgent: "scanner", CreatedAt: now.AddDate(0, 0, -1), MergedAt: now},
		{HiveAttributed: true, HiveModel: "claude-fable-5", HiveBackend: "claude", HiveAgent: "reviewer", CreatedAt: now.AddDate(0, 0, -2), State: "open"},
		{HiveAttributed: true, HiveModel: "auto", HiveBackend: "bob", HiveAgent: "scanner", CreatedAt: now.AddDate(0, 0, -3), State: "closed", ClosedAt: now},
		{HiveAttributed: true, HiveModel: "sonnet", HiveBackend: "copilot", HiveAgent: "scanner", CreatedAt: now.AddDate(0, 0, -20), MergedAt: now},
		{HiveAttributed: true, HiveModel: "opus", HiveBackend: "claude", HiveAgent: "scanner", CreatedAt: now.AddDate(0, 0, -40), MergedAt: now},
		{HiveAttributed: false, HiveModel: "ignored", CreatedAt: now},
	}
	got := aggregateGovernorPRModels(prs, "7d", now)
	if got.Window != "7d" || got.Total != 3 || got.Unknown != 1 {
		t.Fatalf("7d response = %+v, want total 3 unknown 1", got)
	}
	if len(got.Buckets) != 2 {
		t.Fatalf("7d buckets = %d, want 2: %+v", len(got.Buckets), got.Buckets)
	}
	fable := got.Buckets[0]
	if fable.Model != "claude-fable-5" || fable.PRs != 2 || fable.Merged != 1 || fable.Open != 1 || len(fable.Agents) != 2 {
		t.Fatalf("fable bucket = %+v, want merged/open split with agent breakdown", fable)
	}
	all := aggregateGovernorPRModels(prs, "all", now)
	if all.Total != 5 {
		t.Fatalf("all total = %d, want 5", all.Total)
	}
	thirty := aggregateGovernorPRModels(prs, "30d", now)
	if thirty.Total != 4 {
		t.Fatalf("30d total = %d, want 4", thirty.Total)
	}
}

func TestHandleGovernorPRModelsEmptyAndBadWindow(t *testing.T) {
	s := covApiServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/governor/pr-models?window=bogus", nil)
	rec := httptest.NewRecorder()
	s.handleGovernorPRModels(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var got governorPRModelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Window != governorPRModelsWindow7d || got.Total != 0 || len(got.Buckets) != 0 {
		t.Fatalf("response = %+v, want empty default 7d", got)
	}
}

func TestAggregateGovernorPRModelsReworkStats(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	prs := []ghpkg.PullRequest{
		{
			Repo: "hivecommons/hive", Number: 1, Title: "first pass", URL: "https://github.com/hivecommons/hive/pull/1",
			HiveAttributed: true, HiveModel: "opus", HiveBackend: "copilot", CreatedAt: now.AddDate(0, 0, -1), MergedAt: now,
			Rework: ghpkg.PRReworkStats{FirstPass: true, TimeToMergeMinutes: 60},
		},
		{
			Repo: "hivecommons/hive", Number: 2, Title: "needs fixes", URL: "https://github.com/hivecommons/hive/pull/2",
			HiveAttributed: true, HiveModel: "opus", HiveBackend: "copilot", CreatedAt: now.AddDate(0, 0, -2), MergedAt: now,
			Rework: ghpkg.PRReworkStats{ReviewRounds: 2, FixAttempts: 1, FollowUpCommits: 3, HumanChangeRequests: 1, TimeToMergeMinutes: 180, FixerModels: []string{"sonnet"}},
		},
		{
			Repo: "hivecommons/hive", Number: 3,
			HiveAttributed: true, HiveModel: "opus", HiveBackend: "copilot", CreatedAt: now.AddDate(0, 0, -2), State: "open",
			Rework: ghpkg.PRReworkStats{ReviewRounds: 9, FixAttempts: 9},
		},
	}
	got := aggregateGovernorPRModels(prs, "7d", now)
	if len(got.Buckets) != 1 {
		t.Fatalf("buckets = %d, want 1", len(got.Buckets))
	}
	rework := got.Buckets[0].Rework
	if rework.SamplePRs != 2 || rework.FirstPassMerged != 1 || rework.FirstPassMergeRate != 0.5 {
		t.Fatalf("rework sample/first-pass = %+v, want 2/1/0.5", rework)
	}
	if rework.AvgReviewRounds != 1 || rework.WorstReviewRounds != 2 || rework.AvgFixAttempts != 0.5 || rework.WorstFixAttempts != 1 {
		t.Fatalf("rework rounds/attempts = %+v", rework)
	}
	if rework.MedianTimeToMergeMin != 120 || rework.HumanChangeRequests != 1 {
		t.Fatalf("rework median/human = %+v, want median 120 human 1", rework)
	}
	if len(got.MostReworked) != 2 || got.MostReworked[0].Number != 3 || got.MostReworked[1].Number != 2 {
		t.Fatalf("most reworked = %+v, want open PR #3 then merged PR #2", got.MostReworked)
	}
}
