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

// One canonical model driven by two backends with different rework and two
// efforts plus an effort-less PR: per-source and per-effort figures, and their
// sums match the bucket (hivecommons/hive#18715).
func TestAggregateGovernorPRModelsPerSourceAndEffortRework(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	created := now.AddDate(0, 0, -1)
	prs := []ghpkg.PullRequest{
		{Repo: "o/r", Number: 1, HiveAttributed: true, HiveModel: "claude-opus-5-5", HiveBackend: "claude", HiveEffort: "high", CreatedAt: created, MergedAt: now,
			Rework: ghpkg.PRReworkStats{FirstPass: true}},
		{Repo: "o/r", Number: 2, HiveAttributed: true, HiveModel: "claude-opus-5-5", HiveBackend: "claude", HiveEffort: "High", CreatedAt: created, MergedAt: now,
			Rework: ghpkg.PRReworkStats{ReviewRounds: 1, FixAttempts: 1}},
		{Repo: "o/r", Number: 3, HiveAttributed: true, HiveModel: "anthropic/claude-opus-5-5", HiveBackend: "omp", HiveEffort: "medium", CreatedAt: created, MergedAt: now,
			Rework: ghpkg.PRReworkStats{ReviewRounds: 2, FixAttempts: 2}},
		{Repo: "o/r", Number: 4, HiveAttributed: true, HiveModel: "anthropic/claude-opus-5-5", HiveBackend: "omp", CreatedAt: created, State: "closed", ClosedAt: now},
		{Repo: "o/r", Number: 5, HiveAttributed: true, HiveModel: "anthropic/claude-opus-5-5", HiveBackend: "omp", HiveEffort: "medium", CreatedAt: created, State: "open"},
	}
	got := aggregateGovernorPRModels(prs, "7d", now)
	if len(got.Buckets) != 1 {
		t.Fatalf("buckets = %+v, want one canonical row", got.Buckets)
	}
	b := got.Buckets[0]
	// Existing bucket fields keep their values.
	if b.PRs != 5 || b.Merged != 3 || b.ClosedUnmerged != 1 || b.Open != 1 {
		t.Fatalf("bucket outcomes = %d/%d/%d/%d, want 5/3/1/1", b.PRs, b.Merged, b.ClosedUnmerged, b.Open)
	}
	if b.Rework.SamplePRs != 3 || b.Rework.FirstPassMerged != 1 {
		t.Fatalf("bucket rework = %+v, want sample 3 first pass 1", b.Rework)
	}

	if len(b.Sources) != 2 {
		t.Fatalf("sources = %+v, want 2", b.Sources)
	}
	omp, claude := b.Sources[0], b.Sources[1]
	if omp.Backend != "omp" || omp.Model != "anthropic/claude-opus-5-5" {
		t.Fatalf("sources[0] = %+v, want omp (3 PRs) first", omp)
	}
	if omp.PRs != 3 || omp.Merged != 1 || omp.ClosedUnmerged != 1 || omp.Open != 1 ||
		omp.SamplePRs != 1 || omp.FirstPassMerged != 0 || omp.FirstPassMergeRate != 0 ||
		omp.AvgReviewRounds != 2 || omp.AvgFixAttempts != 2 {
		t.Fatalf("omp source = %+v", omp)
	}
	if claude.Backend != "claude" || claude.PRs != 2 || claude.Merged != 2 || claude.ClosedUnmerged != 0 || claude.Open != 0 ||
		claude.SamplePRs != 2 || claude.FirstPassMerged != 1 || claude.FirstPassMergeRate != 0.5 ||
		claude.AvgReviewRounds != 0.5 || claude.AvgFixAttempts != 0.5 {
		t.Fatalf("claude source = %+v", claude)
	}

	want := []governorPRModelEffort{
		{Effort: "high", PRs: 2, Merged: 2, governorPRModelSliceStats: governorPRModelSliceStats{SamplePRs: 2, FirstPassMerged: 1, FirstPassMergeRate: 0.5, AvgReviewRounds: 0.5, AvgFixAttempts: 0.5}},
		{Effort: "medium", PRs: 2, Merged: 1, governorPRModelSliceStats: governorPRModelSliceStats{Open: 1, SamplePRs: 1, AvgReviewRounds: 2, AvgFixAttempts: 2}},
		{Effort: "unknown", PRs: 1, governorPRModelSliceStats: governorPRModelSliceStats{ClosedUnmerged: 1}},
	}
	if len(b.Efforts) != len(want) {
		t.Fatalf("efforts = %+v, want %+v", b.Efforts, want)
	}
	for i := range want {
		if b.Efforts[i] != want[i] {
			t.Fatalf("efforts[%d] = %+v, want %+v", i, b.Efforts[i], want[i])
		}
	}

	var srcPRs, srcMerged, srcFirst, effPRs, effMerged, effFirst, effClosed, effOpen int
	for _, s := range b.Sources {
		srcPRs, srcMerged, srcFirst = srcPRs+s.PRs, srcMerged+s.Merged, srcFirst+s.FirstPassMerged
	}
	for _, e := range b.Efforts {
		effPRs, effMerged, effFirst = effPRs+e.PRs, effMerged+e.Merged, effFirst+e.FirstPassMerged
		effClosed, effOpen = effClosed+e.ClosedUnmerged, effOpen+e.Open
	}
	if srcPRs != b.PRs || srcMerged != b.Merged || srcFirst != b.Rework.FirstPassMerged {
		t.Fatalf("source sums %d/%d/%d != bucket %d/%d/%d", srcPRs, srcMerged, srcFirst, b.PRs, b.Merged, b.Rework.FirstPassMerged)
	}
	if effPRs != b.PRs || effMerged != b.Merged || effFirst != b.Rework.FirstPassMerged || effClosed != b.ClosedUnmerged || effOpen != b.Open {
		t.Fatalf("effort sums %d/%d/%d/%d/%d != bucket", effPRs, effMerged, effFirst, effClosed, effOpen)
	}

	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Sources []map[string]any `json:"sources"`
		Efforts []map[string]any `json:"efforts"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"model", "backend", "prs", "merged", "closed_unmerged", "open", "first_pass_merged", "first_pass_merge_rate", "avg_review_rounds", "avg_fix_attempts", "sample_prs"} {
		if _, ok := decoded.Sources[0][key]; !ok {
			t.Fatalf("sources[0] JSON lacks %q: %s", key, raw)
		}
	}
	for _, key := range []string{"effort", "prs", "merged", "closed_unmerged", "open", "first_pass_merged", "first_pass_merge_rate", "avg_review_rounds", "avg_fix_attempts"} {
		if _, ok := decoded.Efforts[0][key]; !ok {
			t.Fatalf("efforts[0] JSON lacks %q: %s", key, raw)
		}
	}
}

// TestAggregateGovernorPRModelsReposAndWeeks covers the per-repo and per-week
// slices (#18716): a short repo name with a full URL folds into the full-name
// entry, weeks start Monday 00:00 UTC, totals add back to the bucket, and
// weeks are absent for window=7d.
func TestAggregateGovernorPRModelsReposAndWeeks(t *testing.T) {
	// Wednesday 2026-09-23 12:00 UTC.
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	monday := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	pr := func(repo string, n int, url string, created time.Time, merged bool, rw ghpkg.PRReworkStats) ghpkg.PullRequest {
		p := ghpkg.PullRequest{Repo: repo, Number: n, URL: url, HiveAttributed: true, HiveModel: "opus", HiveBackend: "claude", CreatedAt: created, Rework: rw}
		if merged {
			p.MergedAt = now
		} else {
			p.State = "open"
		}
		return p
	}
	prs := []ghpkg.PullRequest{
		// Week of 2026-09-21: exactly Monday 00:00 UTC, plus one more.
		pr("org/alpha", 1, "https://github.com/org/alpha/pull/1", monday, true, ghpkg.PRReworkStats{FirstPass: true}),
		// Short configured name, full URL: must fold into org/alpha.
		pr("alpha", 2, "https://github.com/org/alpha/pull/2", monday.Add(time.Hour), true, ghpkg.PRReworkStats{ReviewRounds: 2, FixAttempts: 1}),
		// One second before the Monday boundary: previous week (2026-09-14).
		pr("org/beta", 3, "https://github.com/org/beta/pull/3", monday.Add(-time.Second), true, ghpkg.PRReworkStats{FirstPass: true}),
		// Week of 2026-09-07; open, so no rework counted.
		pr("org/alpha", 4, "https://github.com/org/alpha/pull/4", monday.AddDate(0, 0, -13), false, ghpkg.PRReworkStats{ReviewRounds: 9}),
	}

	got := aggregateGovernorPRModels(prs, "30d", now)
	if len(got.Buckets) != 1 {
		t.Fatalf("buckets = %+v, want 1", got.Buckets)
	}
	b := got.Buckets[0]
	if b.PRs != 4 || b.Merged != 3 || b.Rework.FirstPassMerged != 2 {
		t.Fatalf("bucket = %+v, want 4 PRs, 3 merged, 2 first pass", b)
	}

	if len(b.Repos) != 2 {
		t.Fatalf("repos = %+v, want org/alpha and org/beta", b.Repos)
	}
	alpha, beta := b.Repos[0], b.Repos[1]
	if alpha.Repo != "org/alpha" || alpha.PRs != 3 || alpha.Merged != 2 || alpha.Open != 1 || alpha.FirstPassMerged != 1 ||
		alpha.FirstPassMergeRate != 0.5 || alpha.AvgReviewRounds != 1 || alpha.AvgFixAttempts != 0.5 {
		t.Fatalf("org/alpha = %+v", alpha)
	}
	if beta.Repo != "org/beta" || beta.PRs != 1 || beta.Merged != 1 || beta.FirstPassMerged != 1 || beta.FirstPassMergeRate != 1 {
		t.Fatalf("org/beta = %+v", beta)
	}

	wantWeeks := []governorPRModelWeekBucket{
		{WeekStart: "2026-09-07", PRs: 1, Merged: 0},
		{WeekStart: "2026-09-14", PRs: 1, Merged: 1, FirstPassMerged: 1, FirstPassMergeRate: 1},
		{WeekStart: "2026-09-21", PRs: 2, Merged: 2, FirstPassMerged: 1, FirstPassMergeRate: 0.5, AvgReviewRounds: 1, AvgFixAttempts: 0.5},
	}
	if len(b.Weeks) != len(wantWeeks) {
		t.Fatalf("weeks = %+v, want %+v", b.Weeks, wantWeeks)
	}
	for i, w := range wantWeeks {
		if b.Weeks[i] != w {
			t.Fatalf("weeks[%d] = %+v, want %+v", i, b.Weeks[i], w)
		}
	}

	for _, window := range []string{"30d", "all", "7d"} {
		resp := aggregateGovernorPRModels(prs, window, now)
		for _, bk := range resp.Buckets {
			var rp, rm, rf int
			for _, r := range bk.Repos {
				rp, rm, rf = rp+r.PRs, rm+r.Merged, rf+r.FirstPassMerged
			}
			if rp != bk.PRs || rm != bk.Merged || rf != bk.Rework.FirstPassMerged {
				t.Fatalf("%s %s: repos sum %d/%d/%d, bucket %d/%d/%d", window, bk.Model, rp, rm, rf, bk.PRs, bk.Merged, bk.Rework.FirstPassMerged)
			}
			if window == "7d" {
				if bk.Weeks != nil {
					t.Fatalf("7d %s: weeks = %+v, want absent", bk.Model, bk.Weeks)
				}
				continue
			}
			var wp, wm, wf int
			for _, w := range bk.Weeks {
				wp, wm, wf = wp+w.PRs, wm+w.Merged, wf+w.FirstPassMerged
			}
			if wp != bk.PRs || wm != bk.Merged || wf != bk.Rework.FirstPassMerged {
				t.Fatalf("%s %s: weeks sum %d/%d/%d, bucket %d/%d/%d", window, bk.Model, wp, wm, wf, bk.PRs, bk.Merged, bk.Rework.FirstPassMerged)
			}
		}
	}

	raw, err := json.Marshal(aggregateGovernorPRModels(prs, "7d", now))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Buckets []map[string]any `json:"buckets"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, bk := range decoded.Buckets {
		if _, ok := bk["weeks"]; ok {
			t.Fatalf("window=7d JSON carries weeks: %v", bk)
		}
		if _, ok := bk["repos"]; !ok {
			t.Fatalf("window=7d JSON missing repos: %v", bk)
		}
	}
}
