package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOwnerOnlyAgentControlsRejectNonOwners(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"pause", "/api/pause/scanner", (&Server{}).handlePause},
		{"resume", "/api/resume/scanner", (&Server{}).handleResume},
		{"breaker engage", "/api/breaker/engage", (&Server{}).handleBreakerEngage},
		{"breaker release", "/api/breaker/release", (&Server{}).handleBreakerRelease},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			req.Header.Set("X-Hive-Role", "read-write")
			req.SetPathValue("agent", "scanner")
			w := httptest.NewRecorder()

			tc.handler(w, req)

			if w.Code != http.StatusForbidden {
				t.Fatalf("non-owner role status = %d, want 403", w.Code)
			}
		})
	}
}
