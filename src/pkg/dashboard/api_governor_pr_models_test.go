package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestHandleGovernorPRModelsIncludesEffectiveMetrics(t *testing.T) {
	s := covApiServer(t)
	dir := t.TempDir()
	s.SetContributorsDir(dir)
	if s.contributeHub != nil {
		s.contributeHub.taskRunLogFile = filepath.Join(dir, taskRunLogFileName)
	}
	now := time.Now().UTC()
	s.deps.Scheduler = metricsSchedulerStub{actionable: &ghpkg.ActionableResult{
		PRs: ghpkg.PRResult{Attributed: []ghpkg.PullRequest{
			{Repo: "org/repo", Number: 1, HiveAttributed: true, HiveModel: "opus", HiveBackend: "copilot", CreatedAt: now, MergedAt: now, Rework: ghpkg.PRReworkStats{FirstPass: true}},
			{Repo: "org/repo", Number: 2, HiveAttributed: true, HiveModel: "opus", HiveBackend: "copilot", CreatedAt: now, MergedAt: now, Rework: ghpkg.PRReworkStats{FirstPass: true}},
		}},
	}}
	var lines []byte
	for _, rec := range []TaskRunRecord{
		{TS: now.Format(time.RFC3339), Backend: "copilot", Model: "opus", Outcome: outcomeCompleted, PRVerified: true, PRURL: "https://github.com/org/repo/pull/1"},
		{TS: now.Format(time.RFC3339), Backend: "copilot", Model: "opus", Outcome: outcomeCompleted},
		{TS: now.Format(time.RFC3339), Backend: "copilot", Model: "opus", Outcome: outcomeFailed},
	} {
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line...)
		lines = append(lines, '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, taskRunLogFileName), lines, 0o600); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/governor/pr-models?window=all", nil)
	rec := httptest.NewRecorder()
	s.handleGovernorPRModels(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got governorPRModelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.MinMergedPRs != effectiveModelsDefaultMinMergedPRs || len(got.Buckets) != 1 {
		t.Fatalf("response = %+v, want one bucket with min threshold", got)
	}
	eff := got.Buckets[0].Effectiveness
	if eff.Rank != 0 || eff.Eligible || eff.MergedPRs != 2 || eff.FirstPassMergeRate != 1 {
		t.Fatalf("effectiveness PR stats = %+v", eff)
	}
	if eff.RunCount != 3 || eff.VerifiedPRRunRate != float64(1)/3 || eff.FailureRate != float64(1)/3 || eff.NothingToShipRate != float64(1)/3 {
		t.Fatalf("effectiveness run stats = %+v", eff)
	}
	if len(eff.FirstPassMergeRateCI) != 2 || len(eff.VerifiedPRRunRateCI) != 2 || len(eff.FailureRateCI) != 2 || len(eff.NothingToShipRateCI) != 2 {
		t.Fatalf("effectiveness intervals missing: %+v", eff)
	}
	if ci := eff.FailureRateCI; ci[0] > eff.FailureRate || eff.FailureRate > ci[1] {
		t.Fatalf("failure_rate_ci %v does not bracket %v", ci, eff.FailureRate)
	}
	if len(got.Buckets[0].Rework.FirstPassMergeRateCI) != 2 {
		t.Fatalf("rework interval missing: %+v", got.Buckets[0].Rework)
	}
	if !got.Coverage.ClosedPRsPartial || got.Coverage.ClosedPRLookbackDays <= 0 || got.Coverage.RunsSince == "" {
		t.Fatalf("coverage = %+v, want partial closed PRs for window=all and runs_since set", got.Coverage)
	}
	if got.EarliestPRCreatedAt != now.Format(time.RFC3339) {
		t.Fatalf("earliest_pr_created_at = %q, want %q", got.EarliestPRCreatedAt, now.Format(time.RFC3339))
	}
}

func TestHandleGovernorPRModelsOmitsIntervalsWithoutRuns(t *testing.T) {
	s := covApiServer(t)
	dir := t.TempDir()
	s.SetContributorsDir(dir)
	if s.contributeHub != nil {
		s.contributeHub.taskRunLogFile = filepath.Join(dir, taskRunLogFileName)
	}
	now := time.Now().UTC()
	s.deps.Scheduler = metricsSchedulerStub{actionable: &ghpkg.ActionableResult{
		PRs: ghpkg.PRResult{Attributed: []ghpkg.PullRequest{
			{Repo: "org/repo", Number: 1, HiveAttributed: true, HiveModel: "opus", HiveBackend: "copilot", CreatedAt: now, MergedAt: now, Rework: ghpkg.PRReworkStats{FirstPass: true}},
		}},
	}}
	rec := httptest.NewRecorder()
	s.handleGovernorPRModels(rec, httptest.NewRequest(http.MethodGet, "/api/governor/pr-models?window=7d", nil))
	var raw struct {
		Coverage map[string]any `json:"coverage"`
		Buckets  []struct {
			Effectiveness map[string]any `json:"effectiveness"`
		} `json:"buckets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if raw.Coverage == nil || len(raw.Buckets) != 1 {
		t.Fatalf("response = %s", rec.Body.String())
	}
	eff := raw.Buckets[0].Effectiveness
	if _, ok := eff["first_pass_merge_rate_ci"]; !ok {
		t.Fatalf("first_pass_merge_rate_ci missing with merged PRs: %v", eff)
	}
	for _, k := range []string{"verified_pr_run_rate_ci", "failure_rate_ci", "nothing_to_ship_rate_ci"} {
		if _, ok := eff[k]; ok {
			t.Fatalf("%s must be omitted when runs == 0: %v", k, eff)
		}
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
