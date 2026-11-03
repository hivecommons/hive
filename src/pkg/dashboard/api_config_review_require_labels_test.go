package dashboard

import (
	"net/http"
	"testing"
)

// PUT /api/config/review replaces review.require_labels whole, normalizes it,
// and an empty list turns the gate off; an absent key leaves it alone.
func TestReviewConfigPut_RequireLabels(t *testing.T) {
	s := covApiServer(t)

	rec := doPut(s, "/api/config/review", map[string]any{"require_labels": []string{" hive-review ", "", "hive-review", "approved-for-agents"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	got := s.deps.Config.Review.RequireLabels
	if len(got) != 2 || got[0] != "hive-review" || got[1] != "approved-for-agents" {
		t.Fatalf("require_labels not normalized: %v", got)
	}

	if rec := doPut(s, "/api/config/review", map[string]any{"priority_labels": true}); rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if len(s.deps.Config.Review.RequireLabels) != 2 {
		t.Fatalf("absent key must leave require_labels untouched: %v", s.deps.Config.Review.RequireLabels)
	}

	if rec := doPut(s, "/api/config/review", map[string]any{"require_labels": []string{}}); rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if s.deps.Config.Review.RequireLabelsEnabled() {
		t.Fatalf("empty list must turn the gate off: %v", s.deps.Config.Review.RequireLabels)
	}

	get := doGetNoRole(s, "/api/config/review")
	if get.Code != http.StatusOK {
		t.Fatalf("GET expected 200, got %d", get.Code)
	}
}
