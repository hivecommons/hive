package hub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMigrateRouteIsGone pins the removal of POST /api/saas/hives/{id}/migrate.
//
// A GET-only catch-all serves the dashboard, so an unrouted POST under
// /api/saas/hives/... surfaces as 405 with "Allow: GET, HEAD" rather than 404 —
// which is itself the proof that no POST handler is registered for this path.
// Accept either, and fail loudly on anything that indicates a handler actually
// ran (2xx, or the 400/403/409 the old handler used to return).
func TestMigrateRouteIsGone(t *testing.T) {
	t.Cleanup(helperSetupTempDirs(t))
	srv := NewHubServer(0, discardLogger(), "test", "v2")

	req := httptest.NewRequest(http.MethodPost, "/api/saas/hives/hosted-x/migrate",
		strings.NewReader(`{"target_cluster_id":"c2"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("migrate route must no longer be served: got status %d, want 404 or 405; body=%q", w.Code, w.Body.String())
	}
}
