package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestChatActorAuthenticationAndAudit(t *testing.T) {
	for _, tc := range []struct {
		name, token, auth, internal, actor, session, proxy, path, want string
		direct                                                         bool
		status                                                         int
	}{
		{name: "chat alice", token: "secret", auth: "Bearer secret", actor: "slack:alice", want: "slack:alice"},
		{name: "chat bob", token: "secret", auth: "Bearer secret", actor: "discord:bob", want: "discord:bob"},
		{name: "ordinary token", token: "secret", auth: "Bearer secret", want: "owner"},
		{name: "invalid token", token: "secret", auth: "Bearer wrong", actor: "slack:alice", status: 401},
		{name: "no token", token: "secret", actor: "slack:alice", status: 401},
		{name: "open deployment", actor: "slack:alice", want: "owner"},
		{name: "public endpoint", token: "secret", auth: "Bearer secret", actor: "slack:alice", path: "/api/health", want: "local"},
		{name: "session wins", token: "secret", auth: "Bearer secret", actor: "slack:alice", session: "viewer", want: "viewer"},
		{name: "proxy wins", token: "secret", auth: "Bearer secret", actor: "slack:alice", proxy: "proxy-user", want: "proxy-user"},
		{name: "direct route rejects token", token: "secret", auth: "Bearer secret", actor: "slack:alice", direct: true, status: 401},
		{name: "internal chat alice", token: "secret", internal: "secret", actor: "slack:alice", want: "slack:alice"},
		{name: "internal direct route chat bob", token: "secret", internal: "secret", actor: "discord:bob", direct: true, want: "discord:bob"},
		{name: "internal ordinary", token: "secret", internal: "secret", want: "internal"},
		{name: "internal invalid token", token: "secret", internal: "wrong", actor: "slack:alice", status: 401},
		{name: "internal session wins", token: "secret", internal: "secret", actor: "slack:alice", session: "viewer", want: "viewer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{authToken: tc.token, audit: &AuditLog{}, userSessions: map[string]*userSession{}}
			if tc.direct {
				s.deps = &Dependencies{Config: &config.Config{Dashboard: config.DashboardConfig{AuthorizedUsers: []string{"owner"}}}}
			}
			path := tc.path
			if path == "" {
				path = "/api/kick"
			}
			r := httptest.NewRequest(http.MethodPost, path, nil)
			r.Header.Set("Authorization", tc.auth)
			r.Header.Set("X-Hive-Internal", tc.internal)
			r.Header.Set("X-Hive-Chat-Actor", tc.actor)
			if tc.session != "" {
				r.AddCookie(&http.Cookie{Name: "hive_session", Value: s.createUserSession(tc.session, config.RoleRead)})
			}
			if tc.proxy != "" {
				r.Header.Set("X-Hive-User", tc.proxy)
				r.Header.Set("X-Hive-Role", config.RoleRead)
				r.Header.Set(proxyAuthHeader, tc.token)
			}
			called := false
			h := s.authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				if got := requestUser(r); got != tc.want {
					t.Errorf("user = %q, want %q", got, tc.want)
				}
				s.auditFromRequest(r, "checkpoint_approve", "", "worker")
				if got := s.audit.Recent(1)[0].User; got != tc.want {
					t.Errorf("audit user = %q", got)
				}
				if (tc.session != "" || tc.proxy != "") && r.Header.Get("X-Hive-Role") != config.RoleRead {
					t.Error("chat actor elevated role")
				}
			}))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			status := tc.status
			if status == 0 {
				status = 200
			}
			if w.Code != status {
				t.Fatalf("status = %d, want %d", w.Code, status)
			}
			if tc.status != 0 && called {
				t.Fatal("unauthenticated handler called")
			}
		})
	}
}
