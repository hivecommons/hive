package dashboard

import (
	"net/http"
	"testing"
)

// review.verdict_labels (hivecommons/hive#13833) round-trips through
// PUT /api/config/review, is replaced whole, and rejects duplicate labels.
func TestReviewConfigPutVerdictLabels(t *testing.T) {
	s := covApiServer(t)
	dup := map[string]any{"verdict_labels": map[string]any{"approve": "done", "requires_human": "DONE"}}
	if rec := doPut(s, "/api/config/review", dup); rec.Code != http.StatusBadRequest {
		t.Fatalf("duplicate labels: expected 400, got %d", rec.Code)
	}
	body := map[string]any{"verdict_labels": map[string]any{
		"reviewing":         " hive-review/in-progress ",
		"approve":           "hive-review/approved",
		"changes_requested": "hive-review/changes-requested",
		"requires_human":    "hive-review/needs-human",
	}}
	if rec := doPut(s, "/api/config/review", body); rec.Code != http.StatusOK {
		t.Fatalf("verdict_labels PUT: %d — %s", rec.Code, rec.Body.String())
	}
	got := s.deps.Config.Review.VerdictLabels
	if got.Reviewing != "hive-review/in-progress" || got.Approve != "hive-review/approved" || got.ChangesRequested != "hive-review/changes-requested" || got.RequiresHuman != "hive-review/needs-human" {
		t.Fatalf("verdict_labels = %+v", got)
	}
	if rec := doPut(s, "/api/config/review", map[string]any{"post_comments": true}); rec.Code != http.StatusOK || !s.deps.Config.Review.VerdictLabels.Enabled() {
		t.Fatalf("an unrelated PUT changed verdict_labels: %+v", s.deps.Config.Review.VerdictLabels)
	}
	if rec := doPut(s, "/api/config/review", map[string]any{"verdict_labels": map[string]any{}}); rec.Code != http.StatusOK || s.deps.Config.Review.VerdictLabels.Enabled() {
		t.Fatalf("empty verdict_labels did not turn the feature off: %+v", s.deps.Config.Review.VerdictLabels)
	}
}
