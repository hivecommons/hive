package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestChatAuthPathsInjectVerifiedUserIdentity(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(*Server)
		decorate   func(*http.Request, *Server)
		wantStatus int
		wantUser   string
		wantRole   string
	}{
		{
			name: "open dev owner fallback",
			setup: func(s *Server) {
				s.authToken = ""
				s.deps.Config.Dashboard = config.DashboardConfig{}
			},
			wantStatus: http.StatusOK,
			wantUser:   "owner",
			wantRole:   config.RoleOwner,
		},
		{
			name: "open dev explicit owner role without user",
			setup: func(s *Server) {
				s.authToken = ""
				s.deps.Config.Dashboard = config.DashboardConfig{}
			},
			decorate: func(r *http.Request, _ *Server) {
				r.Header.Set("X-Hive-Role", config.RoleOwner)
			},
			wantStatus: http.StatusOK,
			wantUser:   "owner",
			wantRole:   config.RoleOwner,
		},
		{
			name: "open dev explicit operator role without user",
			setup: func(s *Server) {
				s.authToken = ""
				s.deps.Config.Dashboard = config.DashboardConfig{}
			},
			decorate: func(r *http.Request, _ *Server) {
				r.Header.Set("X-Hive-Role", config.RoleReadWrite)
			},
			wantStatus: http.StatusOK,
			wantUser:   "owner",
			wantRole:   config.RoleReadWrite,
		},
		{
			name: "internal token uses configured owner",
			setup: func(s *Server) {
				s.authToken = "chat-identity-token"
				s.deps.Config.Dashboard.AuthorizedUsers = []string{"github:alice:owner"}
			},
			decorate: func(r *http.Request, s *Server) {
				r.Header.Set("X-Hive-Internal", s.authToken)
			},
			wantStatus: http.StatusOK,
			wantUser:   "github:alice",
			wantRole:   config.RoleOwner,
		},
		{
			name: "internal token without configured owner uses internal actor",
			setup: func(s *Server) {
				s.authToken = "chat-identity-token"
				s.deps.Config.Dashboard = config.DashboardConfig{}
			},
			decorate: func(r *http.Request, s *Server) {
				r.Header.Set("X-Hive-Internal", s.authToken)
			},
			wantStatus: http.StatusOK,
			wantUser:   "internal",
			wantRole:   config.RoleOwner,
		},
		{
			name: "bearer token uses configured owner",
			setup: func(s *Server) {
				s.authToken = "chat-identity-token"
				s.deps.Config.Dashboard.AuthorizedUsers = []string{"octocat:owner"}
				s.deps.Config.Dashboard.HubProxied = true
			},
			decorate: func(r *http.Request, s *Server) {
				r.Header.Set("Authorization", "Bearer "+s.authToken)
			},
			wantStatus: http.StatusOK,
			wantUser:   "octocat",
			wantRole:   config.RoleOwner,
		},
		{
			name: "unauthenticated request has no verified identity",
			setup: func(s *Server) {
				s.authToken = "chat-identity-token"
			},
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newFullServer(t)
			tt.setup(s)

			var sawUser, sawRole string
			handler := s.authenticate(recordingHandler(&sawUser, &sawRole))
			req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
			if tt.decorate != nil {
				tt.decorate(req, s)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%q", w.Code, tt.wantStatus, w.Body.String())
			}
			if sawUser != tt.wantUser {
				t.Fatalf("X-Hive-User seen by chat handler = %q, want %q", sawUser, tt.wantUser)
			}
			if sawRole != tt.wantRole {
				t.Fatalf("X-Hive-Role seen by chat handler = %q, want %q", sawRole, tt.wantRole)
			}
		})
	}
}
