package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func withHubAdminsPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := hubAdminsPath
	hubAdminsPath = filepath.Join(dir, "hub-admins.json")
	hubAdminGrants.Lock()
	hubAdminGrants.loaded = false
	hubAdminGrants.path = ""
	hubAdminGrants.admins = map[string]persistedHubAdmin{}
	hubAdminGrants.Unlock()
	t.Cleanup(func() {
		hubAdminsPath = old
		hubAdminGrants.Lock()
		hubAdminGrants.loaded = false
		hubAdminGrants.path = ""
		hubAdminGrants.admins = map[string]persistedHubAdmin{}
		hubAdminGrants.Unlock()
	})
	return hubAdminsPath
}

func hubAdminAPIServer() *HubServer {
	s := newHandlerHub()
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("GET /api/hub/admins", s.requireAdmin(s.handleHubAdminsList))
	s.mux.HandleFunc("POST /api/hub/admins", s.requireAdmin(s.handleHubAdminsGrant))
	s.mux.HandleFunc("DELETE /api/hub/admins/{id}", s.requireAdmin(s.handleHubAdminsRevoke))
	s.mux.HandleFunc("GET /api/saas/admin/users", s.requireAdmin(s.handleAdminUsers))
	return s
}

func serveHubAdminAPI(s *HubServer, req *http.Request) *httptest.ResponseRecorder {
	if req.Method != http.MethodGet && req.Header.Get("Origin") == "" {
		req.Header.Set("Origin", "https://hive.hivecommons.dev")
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func TestHubAdminGrantAPI(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	withHubAdminsPath(t)
	t.Setenv(hubAdminsEnv, "")
	s := hubAdminAPIServer()
	mkUser(t, hubAdminUsername)
	mkUser(t, "github:alice")
	mkUser(t, "bob")

	tests := []struct {
		name   string
		method string
		path   string
		body   string
		user   string
		pathID string
		want   int
	}{
		{name: "non-admin get forbidden", method: http.MethodGet, path: "/api/hub/admins", user: "bob", want: http.StatusForbidden},
		{name: "root grants bare login", method: http.MethodPost, path: "/api/hub/admins", body: `{"login":"alice"}`, user: hubAdminUsername, want: http.StatusOK},
		{name: "root grant idempotent", method: http.MethodPost, path: "/api/hub/admins", body: `{"id":"github:alice"}`, user: hubAdminUsername, want: http.StatusOK},
		{name: "granted admin cannot grant", method: http.MethodPost, path: "/api/hub/admins", body: `{"login":"bob"}`, user: "github:alice", want: http.StatusForbidden},
		{name: "granted admin cannot revoke", method: http.MethodDelete, path: "/api/hub/admins/github:alice", user: "github:alice", pathID: "github:alice", want: http.StatusForbidden},
		{name: "root cannot be revoked", method: http.MethodDelete, path: "/api/hub/admins/github:" + hubAdminUsername, user: hubAdminUsername, pathID: "github:" + hubAdminUsername, want: http.StatusForbidden},
		{name: "root revokes granted", method: http.MethodDelete, path: "/api/hub/admins/github:alice", user: hubAdminUsername, pathID: "github:alice", want: http.StatusOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := reqWithUser(tc.method, tc.path, tc.body, tc.user)
			if tc.pathID != "" {
				req.SetPathValue("id", tc.pathID)
			}
			rec := serveHubAdminAPI(s, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
	if isHubAdmin("github:alice") {
		t.Fatal("alice should no longer be admin after revoke")
	}
}

func TestGrantedHubAdminGetsExistingAdminEndpoint(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	withHubAdminsPath(t)
	t.Setenv(hubAdminsEnv, "")
	s := hubAdminAPIServer()
	mkUser(t, hubAdminUsername)
	mkUser(t, "github:alice")

	rec := serveHubAdminAPI(s, reqWithUser(http.MethodPost, "/api/hub/admins", `{"login":"alice"}`, hubAdminUsername))
	if rec.Code != http.StatusOK {
		t.Fatalf("grant status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = serveHubAdminAPI(s, reqWithUser(http.MethodGet, "/api/saas/admin/users", "", "github:alice"))
	if rec.Code != http.StatusOK {
		t.Fatalf("granted admin endpoint status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHubAdminGrantPersistenceReloadAndCanonicalization(t *testing.T) {
	withHubAdminsPath(t)
	t.Setenv(hubAdminsEnv, "")
	admins := loadGrantedHubAdmins()
	admins[hubAdminKey("github:carol")] = persistedHubAdmin{ID: "github:carol", GrantedBy: "github:" + hubAdminUsername, GrantedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := saveGrantedHubAdmins(admins); err != nil {
		t.Fatalf("saveGrantedHubAdmins: %v", err)
	}
	hubAdminGrants.Lock()
	hubAdminGrants.loaded = false
	hubAdminGrants.admins = map[string]persistedHubAdmin{}
	hubAdminGrants.Unlock()
	if !isHubAdmin("carol") || !isHubAdmin("github:carol") {
		t.Fatal("persisted bare/canonical github admin did not reload")
	}
	if isRootHubAdmin("carol") {
		t.Fatal("persisted grant must not become root admin")
	}
}

func TestHubAdminGrantImpersonationCannotGrant(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	withHubAdminsPath(t)
	t.Setenv(hubAdminsEnv, "")
	s := hubAdminAPIServer()
	mkUser(t, hubAdminUsername)
	mkUser(t, "alice")
	req := reqWithUser(http.MethodPost, "/api/hub/admins", `{"login":"mallory"}`, hubAdminUsername)
	req.AddCookie(impersonateCookie(hubAdminUsername, "alice", time.Now()))
	rec := serveHubAdminAPI(s, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("impersonated grant status = %d, want 403 body=%s", rec.Code, rec.Body.String())
	}
	if isHubAdmin("mallory") {
		t.Fatal("impersonated session granted admin")
	}
}

func TestHubAdminListPayload(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	withHubAdminsPath(t)
	t.Setenv(hubAdminsEnv, "github:"+hubAdminUsername)
	s := hubAdminAPIServer()
	mkUser(t, hubAdminUsername)
	mkUser(t, "dora")

	if rec := serveHubAdminAPI(s, reqWithUser(http.MethodPost, "/api/hub/admins", `{"login":"dora"}`, hubAdminUsername)); rec.Code != http.StatusOK {
		t.Fatalf("grant dora = %d body=%s", rec.Code, rec.Body.String())
	}
	rec := serveHubAdminAPI(s, reqWithUser(http.MethodGet, "/api/hub/admins", "", "dora"))
	if rec.Code != http.StatusOK {
		t.Fatalf("granted list status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		RootAdmin bool               `json:"root_admin"`
		Admins    []hubAdminAPIEntry `json:"admins"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if got.RootAdmin {
		t.Fatal("granted admin must not see root_admin=true")
	}
	var sawRoot, sawGranted bool
	for _, a := range got.Admins {
		if a.ID == "github:"+hubAdminUsername && a.Root {
			sawRoot = true
		}
		if strings.EqualFold(a.ID, "github:dora") && !a.Root && a.GrantedBy != "" && a.GrantedAt != "" {
			sawGranted = true
		}
	}
	if !sawRoot || !sawGranted {
		t.Fatalf("list missing root/granted entries: %+v", got.Admins)
	}
}
