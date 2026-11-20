package dashboard

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// storedClosedPRs are three merged opus PRs closed 3, 16 and 25 days ago (each
// created a day earlier): all inside a 30d window, the last two past the live
// scan's 14-day lookback.
func storedClosedPRs(now time.Time) []ghpkg.PullRequest {
	var out []ghpkg.PullRequest
	for i, days := range []int{3, 16, 25} {
		closed := now.AddDate(0, 0, -days)
		out = append(out, ghpkg.PullRequest{
			Repo: "org/repo", Number: i + 1, URL: fmt.Sprintf("https://github.com/org/repo/pull/%d", i+1),
			HiveAttributed: true, HiveModel: "opus", HiveBackend: "copilot", State: "closed",
			CreatedAt: closed.AddDate(0, 0, -1), ClosedAt: closed, MergedAt: closed,
			Rework: ghpkg.PRReworkStats{FirstPass: true},
		})
	}
	return out
}

func TestPRModels30dCountsStoredClosedPRsPastLiveLookback(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	stored := storedClosedPRs(now)
	// The live scan still lists the newest one; the merge must not double it.
	live := []ghpkg.PullRequest{stored[0]}
	prs := mergeAttributedPRHistory(live, stored)
	if len(prs) != 3 {
		t.Fatalf("merged PRs = %d, want 3 (live wins, no duplicates)", len(prs))
	}

	got := aggregateGovernorPRModels(prs, governorPRModelsWindow30d, now)
	if got.Total != 3 || len(got.Buckets) != 1 || got.Buckets[0].Merged != 3 || got.Buckets[0].Rework.FirstPassMerged != 3 {
		t.Fatalf("30d PRs-by-model = %+v, want all three merged PRs counted", got)
	}
	eff := aggregateContributeEffectiveModels(prs, nil, governorPRModelsWindow30d, effectiveModelsFilterAll, 1, now)
	if len(eff.Ranked) != 1 || eff.Ranked[0].MergedPRs != 3 || eff.Ranked[0].FirstPassMerged != 3 {
		t.Fatalf("30d effective models = %+v, want one ranked row over three merged PRs", eff)
	}
}

func TestApplyAttributedPRHistoryCoverage(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	month := 30 * day
	base := func() contributeEffectiveModelsCoverage {
		return effectiveModelsCoverage(month, 14*day, nil, false, now)
	}
	if cov := base(); !cov.ClosedPRsPartial {
		t.Fatalf("live-only 30d coverage = %+v, want partial (control)", cov)
	}

	covered := base()
	applyAttributedPRHistoryCoverage(&covered, ghpkg.AttributedPRCoverage{Available: true, Since: now.Add(-31 * day)}, nil, month, now)
	if covered.ClosedPRsPartial || covered.ClosedPRLookbackDays != 31 || covered.ClosedPRsSince != now.Add(-31*day).Format(time.RFC3339) {
		t.Fatalf("coverage with the store back 31 days = %+v, want 30d complete since that date", covered)
	}

	short := base()
	applyAttributedPRHistoryCoverage(&short, ghpkg.AttributedPRCoverage{Available: true, Since: now.Add(-14 * day)}, nil, month, now)
	if !short.ClosedPRsPartial || short.ClosedPRsSince == "" {
		t.Fatalf("coverage during the backfill = %+v, want 30d partial", short)
	}

	all := effectiveModelsCoverage(0, 14*day, nil, false, now)
	applyAttributedPRHistoryCoverage(&all, ghpkg.AttributedPRCoverage{Available: true, Since: now.Add(-31 * day)}, nil, 0, now)
	if !all.ClosedPRsPartial {
		t.Fatalf("All coverage = %+v, want partial before the backfill horizon", all)
	}

	full := effectiveModelsCoverage(0, 14*day, nil, false, now)
	applyAttributedPRHistoryCoverage(&full, ghpkg.AttributedPRCoverage{Available: true, Full: true}, storedClosedPRs(now), 0, now)
	if full.ClosedPRsPartial || full.ClosedPRsSince != now.AddDate(0, 0, -26).Format(time.RFC3339) {
		t.Fatalf("All coverage with full history = %+v, want complete since the oldest PR", full)
	}

	none := base()
	applyAttributedPRHistoryCoverage(&none, ghpkg.AttributedPRCoverage{}, nil, month, now)
	if none != base() {
		t.Fatalf("coverage without a store = %+v, want the live scan's unchanged", none)
	}
}

// seedAttributedPRHistory writes a history file (the format
// pkg/github/attributed_pr_history.go reads) holding storedClosedPRs for repo
// "repo", with the one-time backfill done or still running.
func seedAttributedPRHistory(t *testing.T, now time.Time, backfillDone bool) string {
	t.Helper()
	ts := func(tm time.Time) string { return tm.UTC().Format(time.RFC3339Nano) }
	records := map[string]any{}
	for _, pr := range storedClosedPRs(now) {
		records[fmt.Sprintf("repo#%d", pr.Number)] = map[string]any{
			"repo": "repo", "number": pr.Number, "url": pr.URL, "model": pr.HiveModel, "backend": pr.HiveBackend,
			"state": "closed", "created_at": ts(pr.CreatedAt), "closed_at": ts(pr.ClosedAt), "merged_at": ts(pr.MergedAt),
			"rework": map[string]any{"first_pass": true}, "rework_known": true,
		}
	}
	scan := now.Add(-time.Hour)
	backfill := map[string]any{"started_at": ts(scan), "horizon": ts(scan.Add(-31 * 24 * time.Hour)), "next_page": 4}
	if backfillDone {
		backfill["listed"], backfill["done"] = true, true
	}
	file := map[string]any{
		"schema_version": 1,
		"generated_at":   ts(now),
		"repos": map[string]any{"repo": map[string]any{
			"last_scan_at": ts(scan), "run_start": ts(scan), "run_floor": ts(scan.Add(-14 * 24 * time.Hour)), "backfill": backfill,
		}},
		"records": records,
	}
	raw, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), ghpkg.AttributedPRHistoryFile)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func prModelsHistoryServer(t *testing.T, historyPath string) *Server {
	t.Helper()
	s := covApiServer(t)
	dir := t.TempDir()
	s.SetContributorsDir(dir)
	if s.contributeHub != nil {
		s.contributeHub.taskRunLogFile = filepath.Join(dir, taskRunLogFileName)
	}
	client := ghpkg.NewClientForTest("http://127.0.0.1:1", "org", []string{"repo"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	client.SetAttributedPRHistoryPath(historyPath)
	s.deps.GHClient = client
	s.deps.Scheduler = metricsSchedulerStub{actionable: &ghpkg.ActionableResult{}}
	return s
}

func TestPRModelsHandlersReadAttributedPRHistory(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name        string
		done        bool
		window      string
		wantPartial bool
	}{
		{"backfill done, 30d", true, "30d", false},
		{"backfill running, 30d", false, "30d", true},
		{"backfill done, all", true, "all", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := prModelsHistoryServer(t, seedAttributedPRHistory(t, now, tc.done))

			rec := httptest.NewRecorder()
			s.handleGovernorPRModels(rec, httptest.NewRequest(http.MethodGet, "/api/governor/pr-models?window="+tc.window, nil))
			var models governorPRModelsResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &models); err != nil {
				t.Fatalf("decode pr-models: %v body %s", err, rec.Body.String())
			}
			if models.Total != 3 || len(models.Buckets) != 1 || models.Buckets[0].Merged != 3 {
				t.Fatalf("pr-models = %+v, want the three stored merged PRs", models)
			}
			if models.Coverage.ClosedPRsPartial != tc.wantPartial || models.Coverage.ClosedPRsSince == "" {
				t.Fatalf("pr-models coverage = %+v, want partial=%v with closed_prs_since", models.Coverage, tc.wantPartial)
			}

			rec = httptest.NewRecorder()
			s.handleContributeEffectiveModels(rec, httptest.NewRequest(http.MethodGet, "/api/contribute/effective-models?window="+tc.window, nil))
			var eff contributeEffectiveModelsResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &eff); err != nil {
				t.Fatalf("decode effective-models: %v body %s", err, rec.Body.String())
			}
			merged := 0
			for _, row := range append(eff.Ranked, eff.Insufficient...) {
				merged += row.MergedPRs
			}
			if merged != 3 {
				t.Fatalf("effective-models = %+v, want three merged PRs", eff)
			}
			if eff.Coverage.ClosedPRsPartial != tc.wantPartial {
				t.Fatalf("effective-models coverage = %+v, want partial=%v", eff.Coverage, tc.wantPartial)
			}
		})
	}
}
