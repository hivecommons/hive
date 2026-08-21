package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The fleet page moved from /my-hives to /fleet to match the "Fleet" nav
// button that opens it. /fleet must serve the page; /my-hives must
// permanently redirect (query string preserved) so old bookmarks, shared
// links, and OAuth redirect params keep working. The API endpoint
// /api/saas/my-hives is intentionally unchanged.
func TestFleetRouteServesPageAndMyHivesRedirects(t *testing.T) {
	t.Setenv("HUB_DATA_DIR", t.TempDir())
	srv := NewHubServer(0, slog.Default(), "test", "v4")

	req := httptest.NewRequest("GET", "/fleet", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /fleet: expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "<html") {
		t.Error("GET /fleet: expected the fleet page HTML shell")
	}

	req = httptest.NewRequest("GET", "/my-hives?view=quadrant", nil)
	w = httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusMovedPermanently {
		t.Fatalf("GET /my-hives: expected 301, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/fleet?view=quadrant" {
		t.Errorf("GET /my-hives: Location = %q, want /fleet?view=quadrant", loc)
	}
}
