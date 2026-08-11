package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBeadsResetHandlersRejectNonOwners(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"all agents", "/api/beads/reset", newFullServer(t).handleBeadsReset},
		{"single agent", "/api/beads/scanner/reset", newFullServer(t).handleBeadsResetAgent},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			req.Header.Set("X-Hive-Role", "read-write")
			req.SetPathValue("agent", "scanner")
			w := httptest.NewRecorder()

			tc.handler(w, req)

			if w.Code != http.StatusForbidden {
				t.Fatalf("non-owner status = %d, want %d", w.Code, http.StatusForbidden)
			}
		})
	}
}
