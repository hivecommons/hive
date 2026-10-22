package dashboard

import (
	"net/http"
	"testing"
)

// review.head_max_retries (hivecommons/hive#13542) round-trips through
// PUT /api/config/review and rejects a negative value like max_redispatch.
func TestReviewConfigPutHeadMaxRetries(t *testing.T) {
	s := covApiServer(t)
	if rec := doPut(s, "/api/config/review", map[string]any{"head_max_retries": -1}); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative head_max_retries: expected 400, got %d", rec.Code)
	}
	if rec := doPut(s, "/api/config/review", map[string]any{"head_max_retries": 2}); rec.Code != http.StatusOK {
		t.Fatalf("head_max_retries PUT: %d — %s", rec.Code, rec.Body.String())
	}
	if got := s.deps.Config.Review.HeadMaxRetries; got != 2 {
		t.Fatalf("head_max_retries = %d, want 2", got)
	}
	if rec := doPut(s, "/api/config/review", map[string]any{"post_comments": true}); rec.Code != http.StatusOK || s.deps.Config.Review.HeadMaxRetries != 2 {
		t.Fatalf("an unrelated PUT changed head_max_retries: %d", s.deps.Config.Review.HeadMaxRetries)
	}
}
