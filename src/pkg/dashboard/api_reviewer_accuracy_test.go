package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/review"
)

func seedReviewerAccuracyData(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	origVerdicts := review.ReviewVerdictsPath
	origOutcomes := review.ReviewOutcomesPath
	review.ReviewVerdictsPath = dir + "/review-verdicts.json"
	review.ReviewOutcomesPath = dir + "/review-outcomes.json"
	t.Cleanup(func() {
		review.ReviewVerdictsPath = origVerdicts
		review.ReviewOutcomesPath = origOutcomes
	})
	now := time.Now().UTC()
	artifact := review.Artifact{Items: []review.Aggregate{
		{
			Repo: "acme/a", Number: 1, HeadSHA: "a", ReviewModel: "reviewer-a", Verdict: review.VerdictChangesRequested,
			Perspectives: map[review.Perspective]review.Verdict{review.PerspectiveCorrectness: review.VerdictChangesRequested},
			Confidence:   review.Confidence{Score: 2}, RecordedAt: now.Add(-time.Hour),
		},
	}}
	if err := review.WriteArtifact("", artifact); err != nil {
		t.Fatalf("WriteArtifact: %v", err)
	}
	ledger := &review.OutcomeLedger{Items: map[string]*review.PROutcome{
		"acme/a#1": {Repo: "acme/a", Number: 1, FirstSeenAt: now.Add(-2 * time.Hour), Outcome: review.OutcomeMerged, OutcomeAt: now.Add(-time.Hour)},
	}}
	if err := ledger.Save("", now); err != nil {
		t.Fatalf("Save outcomes: %v", err)
	}
}

func TestHandleReviewerAccuracy(t *testing.T) {
	seedReviewerAccuracyData(t)
	s := covApiServer(t)
	rec := httptest.NewRecorder()
	s.handleReviewerAccuracy(rec, httptest.NewRequest(http.MethodGet, "/api/reviewer/accuracy?days=7", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var sum review.ReviewerAccuracySummary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sum.WindowDays != 7 || sum.Samples != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	if len(sum.Perspectives) != 1 || sum.Perspectives[0].Blocks != 1 || sum.Perspectives[0].FalseBlocks != 0 {
		t.Fatalf("perspectives = %+v", sum.Perspectives)
	}
	if len(sum.ReviewerModels) != 1 || sum.ReviewerModels[0].Name != "reviewer-a" {
		t.Fatalf("models = %+v", sum.ReviewerModels)
	}
}

func TestReviewerAccuracyWindowDaysUsesEnvAndRejectsBadInput(t *testing.T) {
	t.Setenv(reviewerAccuracyWindowEnv, "14")
	if got, err := reviewerAccuracyWindowDays(""); err != nil || got != 14 {
		t.Fatalf("env window = %d, %v", got, err)
	}
	if _, err := reviewerAccuracyWindowDays("91"); err == nil {
		t.Fatal("days=91 accepted")
	}
	if _, err := reviewerAccuracyWindowDays("abc"); err == nil {
		t.Fatal("days=abc accepted")
	}
}

func TestHandleReviewerAccuracy_MissingVerdictsIsEmpty(t *testing.T) {
	dir := t.TempDir()
	origVerdicts := review.ReviewVerdictsPath
	origOutcomes := review.ReviewOutcomesPath
	review.ReviewVerdictsPath = dir + "/missing-verdicts.json"
	review.ReviewOutcomesPath = dir + "/missing-outcomes.json"
	t.Cleanup(func() {
		review.ReviewVerdictsPath = origVerdicts
		review.ReviewOutcomesPath = origOutcomes
	})
	s := covApiServer(t)
	rec := httptest.NewRecorder()
	s.handleReviewerAccuracy(rec, httptest.NewRequest(http.MethodGet, "/api/reviewer/accuracy", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var sum review.ReviewerAccuracySummary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sum.Samples != 0 {
		t.Fatalf("samples = %d, want 0", sum.Samples)
	}
}
