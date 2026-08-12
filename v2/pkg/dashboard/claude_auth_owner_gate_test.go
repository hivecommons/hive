package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClaudeAuthMutationsRequireOwnerRole(t *testing.T) {
	stubClaudeSeams(t)

	for _, tc := range []struct {
		name   string
		path   string
		body   string
		invoke func(*Server, http.ResponseWriter, *http.Request)
	}{
		{
			name:   "start",
			path:   "/api/claude-auth/start",
			invoke: (*Server).handleClaudeAuthStart,
		},
		{
			name:   "exchange",
			path:   "/api/claude-auth/exchange",
			body:   `{"code":"test-code"}`,
			invoke: (*Server).handleClaudeAuthExchange,
		},
		{
			name:   "logout",
			path:   "/api/claude-auth/logout",
			invoke: (*Server).handleClaudeAuthLogout,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, role := range []string{"", "read-write"} {
				t.Run("denies_"+role, func(t *testing.T) {
					s := claudeDeepServer(t)
					rec := httptest.NewRecorder()
					req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
					if role != "" {
						req.Header.Set("X-Hive-Role", role)
					}

					tc.invoke(s, rec, req)

					if rec.Code != http.StatusForbidden {
						t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
					}
				})
			}

			t.Run("allows_owner", func(t *testing.T) {
				s := claudeDeepServer(t)
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
				markOwnerRequest(req)

				tc.invoke(s, rec, req)

				if rec.Code == http.StatusForbidden {
					t.Fatalf("owner was denied: body=%s", rec.Body.String())
				}
			})
		})
	}
}
