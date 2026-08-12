package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCopilotAuthMutationsRequireOwnerRole(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   string
		invoke func(*Server, http.ResponseWriter, *http.Request)
	}{
		{
			name:   "start",
			path:   "/api/copilot-auth/start",
			invoke: (*Server).handleCopilotAuthStart,
		},
		{
			name:   "logout",
			path:   "/api/copilot-auth/logout",
			invoke: (*Server).handleCopilotAuthLogout,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, role := range []string{"", "read-write"} {
				s := covApiServer(t)
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, tc.path, nil)
				if role != "" {
					req.Header.Set("X-Hive-Role", role)
				}

				tc.invoke(s, rec, req)

				if rec.Code != http.StatusForbidden {
					t.Fatalf("role %q: status = %d, want 403", role, rec.Code)
				}
			}
		})
	}
}
