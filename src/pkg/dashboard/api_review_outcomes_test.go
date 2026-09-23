package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/review"
)

// seedOutcomeLedger writes a small ledger: one reviewed PR merged in 6h, one
// unreviewed PR merged in 30h, one unreviewed PR still open.
func seedOutcomeLedger(t *testing.T) {
	t.Helper()
	orig := review.ReviewOutcomesPath
	review.ReviewOutcomesPath = t.TempDir() + "/review-outcomes.json"
	t.Cleanup(func() { review.ReviewOutcomesPath = orig })

	now := time.Now().UTC()
	t0 := now.Add(-48 * time.Hour)
	l := &review.OutcomeLedger{Items: map[string]*review.PROutcome{}}
	open := []review.OpenPR{
		{Repo: "acme/a", Number: 1, Author: "bot", AgentAuthored: true, CreatedAt: t0},
		{Repo: "acme/a", Number: 2, Author: "alice", CreatedAt: t0},
		{Repo: "acme/a", Number: 3, Author: "bob", CreatedAt: t0},
	}
	l.Observe(t0, open, nil)
	l.Observe(t0.Add(time.Hour), open, map[string]review.ReviewSignal{
		"acme/a#1": {At: t0.Add(time.Hour), Verdict: review.VerdictApprove},
	})
	l.Resolve("acme/a", 1, "closed", t0.Add(6*time.Hour), t0.Add(6*time.Hour))
	l.Resolve("acme/a", 2, "closed", t0.Add(30*time.Hour), t0.Add(30*time.Hour))
	if err := l.Save("", now); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

func TestHandleReviewOutcomes(t *testing.T) {
	seedOutcomeLedger(t)
	s := covApiServer(t)

	rec := httptest.NewRecorder()
	s.handleReviewOutcomes(rec, httptest.NewRequest(http.MethodGet, "/api/review/outcomes?days=7", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var sum review.OutcomeSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sum.WindowDays != 7 {
		t.Fatalf("window_days = %d, want 7", sum.WindowDays)
	}
	if sum.Reviewed.PRs != 1 || sum.Reviewed.Merged != 1 || sum.Reviewed.MedianHoursSeenToMerge != 6 {
		t.Fatalf("reviewed cohort: %+v", sum.Reviewed)
	}
	if sum.Unreviewed.PRs != 2 || sum.Unreviewed.Merged != 1 || sum.Unreviewed.Open != 1 || sum.Unreviewed.MedianHoursSeenToMerge != 30 {
		t.Fatalf("unreviewed cohort: %+v", sum.Unreviewed)
	}
	if sum.ByVerdict[review.VerdictApprove].Merged != 1 {
		t.Fatalf("by_verdict: %+v", sum.ByVerdict)
	}
}

func TestHandleReviewOutcomes_RejectsBadWindow(t *testing.T) {
	seedOutcomeLedger(t)
	s := covApiServer(t)
	for _, q := range []string{"days=0", "days=91", "days=abc"} {
		rec := httptest.NewRecorder()
		s.handleReviewOutcomes(rec, httptest.NewRequest(http.MethodGet, "/api/review/outcomes?"+q, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", q, rec.Code)
		}
	}
}

func TestHandleReviewOutcomes_EmptyLedgerIsZeroes(t *testing.T) {
	orig := review.ReviewOutcomesPath
	review.ReviewOutcomesPath = t.TempDir() + "/missing.json"
	t.Cleanup(func() { review.ReviewOutcomesPath = orig })
	s := covApiServer(t)
	rec := httptest.NewRecorder()
	s.handleReviewOutcomes(rec, httptest.NewRequest(http.MethodGet, "/api/review/outcomes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"window_days":30`) {
		t.Fatalf("default window not applied: %s", rec.Body.String())
	}
}

func TestHandleMetrics_ReviewOutcomeSeries(t *testing.T) {
	seedOutcomeLedger(t)
	s := covApiServer(t)
	s.deps.Config.HiveID = "test-hive"
	t.Setenv("HIVE_METRICS_TOKEN", "sk-metrics")
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer sk-metrics")
	rec := httptest.NewRecorder()
	s.handleMetrics(rec, req)
	body := rec.Body.String()
	for _, want := range []string{
		`hive_review_outcome_prs{hive_id="test-hive",reviewed="reviewed",outcome="merged"} 1`,
		`hive_review_outcome_prs{hive_id="test-hive",reviewed="unreviewed",outcome="open"} 1`,
		`hive_review_outcome_median_hours_to_merge{hive_id="test-hive",reviewed="reviewed"} 6.00`,
		`hive_review_outcome_median_hours_to_merge{hive_id="test-hive",reviewed="unreviewed"} 30.00`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}
