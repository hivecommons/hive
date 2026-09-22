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
